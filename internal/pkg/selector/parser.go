package selector

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	labelSelectorField = "labelSelector"
	annotationsField   = "resourceAnnotations"
)

// tokenKind enumerates the lexical tokens of a label-selector requirement.
type tokenKind int

const (
	tokenEnd tokenKind = iota
	tokenIdentifier
	tokenBang
	tokenEquals
	tokenDoubleEquals
	tokenNotEquals
	tokenGreaterThan
	tokenLessThan
	tokenOpenParen
	tokenCloseParen
	tokenComma
)

// token is a lexical token plus its byte column in the source requirement.
type token struct {
	kind tokenKind
	text string
	pos  int
}

// isIdentifierRune reports whether ch may appear inside a key or label value.
// Label grammar additionally constrains keys/values, which is validated after
// lexing.
func isIdentifierRune(ch byte) bool {
	if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
		return true
	}
	switch ch {
	case '-', '_', '.', '/':
		return true
	default:
		return false
	}
}

// lex tokenizes a single label-selector requirement. It is only called after
// illegalOperatorToken has rejected unknown characters.
func lex(source string) []token {
	tokens := []token{}
	for index := 0; index < len(source); {
		switch ch := source[index]; ch {
		case ' ', '\t':
			index++
		case '=':
			if index+1 < len(source) && source[index+1] == '=' {
				tokens = append(tokens, token{kind: tokenDoubleEquals, text: "==", pos: index})
				index += 2
			} else {
				tokens = append(tokens, token{kind: tokenEquals, text: "=", pos: index})
				index++
			}
		case '!':
			if index+1 < len(source) && source[index+1] == '=' {
				tokens = append(tokens, token{kind: tokenNotEquals, text: "!=", pos: index})
				index += 2
			} else {
				tokens = append(tokens, token{kind: tokenBang, text: "!", pos: index})
				index++
			}
		case '>':
			tokens = append(tokens, token{kind: tokenGreaterThan, text: ">", pos: index})
			index++
		case '<':
			tokens = append(tokens, token{kind: tokenLessThan, text: "<", pos: index})
			index++
		case '(':
			tokens = append(tokens, token{kind: tokenOpenParen, text: "(", pos: index})
			index++
		case ')':
			tokens = append(tokens, token{kind: tokenCloseParen, text: ")", pos: index})
			index++
		case ',':
			tokens = append(tokens, token{kind: tokenComma, text: ",", pos: index})
			index++
		default:
			start := index
			for index < len(source) && isIdentifierRune(source[index]) {
				index++
			}
			tokens = append(tokens, token{kind: tokenIdentifier, text: source[start:index], pos: start})
		}
	}
	tokens = append(tokens, token{kind: tokenEnd, pos: len(source)})
	return tokens
}

// illegalOperatorToken scans for characters that are not part of the label
// selector grammar and for adjacent operator symbols that form an unsupported
// operator (for example ">=" or "~="). It returns the byte column and literal
// of the first offending token, so the error can be located on the
// requirement's operator field.
func illegalOperatorToken(source string) (int, string) {
	const binarySymbols = "=!><"
	for index := 0; index < len(source); {
		ch := source[index]
		switch {
		case ch == ' ' || ch == '\t' || ch == '(' || ch == ')' || ch == ',':
			index++
		case strings.ContainsRune(binarySymbols, rune(ch)):
			start := index
			for index < len(source) && strings.ContainsRune(binarySymbols, rune(source[index])) {
				index++
			}
			literal := source[start:index]
			if len(literal) >= 2 && literal != "==" && literal != "!=" {
				return start, literal
			}
		case isIdentifierRune(ch):
			index++
		default:
			return index, string(ch)
		}
	}
	return -1, ""
}

// parseLabelRequirement parses one comma-separated requirement of a label
// selector expression. index is the requirement index, used in field paths.
func parseLabelRequirement(raw string, index int) (*term, Diagnostics) {
	base := fmt.Sprintf("%s.requirements[%d]", labelSelectorField, index)
	source := strings.TrimSpace(raw)
	if source == "" {
		return nil, Diagnostics{errorDiagnostic(base, "empty requirement")}
	}

	if column, illegal := illegalOperatorToken(source); illegal != "" {
		return nil, Diagnostics{errorDiagnostic(
			base+".operator",
			fmt.Sprintf("illegal operator %q at column %d; supported operators are: %s", illegal, column+1, supportedOperatorText),
		)}
	}

	tokens := lex(source)
	cursor := 0
	operator := selection.Exists

	if tokens[cursor].kind == tokenBang {
		operator = selection.DoesNotExist
		cursor++
	}

	if tokens[cursor].kind != tokenIdentifier {
		return nil, Diagnostics{errorDiagnostic(
			base+".key",
			fmt.Sprintf("expected a key but found %q", tokens[cursor].text),
		)}
	}
	key := tokens[cursor].text
	cursor++

	if reasons := validation.IsQualifiedName(key); len(reasons) > 0 {
		return nil, Diagnostics{errorDiagnostic(base+".key", strings.Join(reasons, "; "))}
	}

	if operator == selection.DoesNotExist {
		if tokens[cursor].kind != tokenEnd {
			return nil, Diagnostics{errorDiagnostic(
				base+".operator",
				"'doesnotexist' requirement must only name a key and cannot take values",
			)}
		}
		return newTerm(base, key, operator, nil), nil
	}

	switch tokens[cursor].kind {
	case tokenEnd:
		return newTerm(base, key, selection.Exists, nil), nil
	case tokenEquals:
		operator = selection.Equals
	case tokenDoubleEquals:
		operator = selection.DoubleEquals
	case tokenNotEquals:
		operator = selection.NotEquals
	case tokenGreaterThan:
		operator = selection.GreaterThan
	case tokenLessThan:
		operator = selection.LessThan
	case tokenIdentifier:
		switch tokens[cursor].text {
		case string(selection.In):
			operator = selection.In
		case string(selection.NotIn):
			operator = selection.NotIn
		default:
			return nil, Diagnostics{errorDiagnostic(
				base+".operator",
				fmt.Sprintf("unexpected identifier %q where an operator was expected; supported operators are: %s", tokens[cursor].text, supportedOperatorText),
			)}
		}
	default:
		return nil, Diagnostics{errorDiagnostic(
			base+".operator",
			fmt.Sprintf("unexpected token %q; supported operators are: %s", tokens[cursor].text, supportedOperatorText),
		)}
	}
	operatorColumn := tokens[cursor].pos
	cursor++

	if operator == selection.In || operator == selection.NotIn {
		return parseSetValues(base, key, operator, tokens, cursor)
	}
	return parseExactValue(base, key, operator, operatorColumn, tokens, cursor)
}

// parseSetValues parses the "(v1, v2)" value list of an in/notin requirement.
func parseSetValues(base, key string, operator selection.Operator, tokens []token, cursor int) (*term, Diagnostics) {
	if tokens[cursor].kind != tokenOpenParen {
		return nil, Diagnostics{errorDiagnostic(
			base+".values",
			fmt.Sprintf("expected '(' after %q but found %q", operator, tokens[cursor].text),
		)}
	}
	cursor++

	values := []string{}
	if tokens[cursor].kind != tokenCloseParen {
		for {
			if tokens[cursor].kind != tokenIdentifier {
				return nil, Diagnostics{errorDiagnostic(
					base+".values",
					fmt.Sprintf("expected a value but found %q", tokens[cursor].text),
				)}
			}
			values = append(values, tokens[cursor].text)
			cursor++

			switch tokens[cursor].kind {
			case tokenComma:
				cursor++
				if tokens[cursor].kind == tokenCloseParen {
					return nil, Diagnostics{errorDiagnostic(base+".values", "trailing comma in value list")}
				}
			case tokenCloseParen:
				cursor++
				goto valuesParsed
			default:
				return nil, Diagnostics{errorDiagnostic(
					base+".values",
					fmt.Sprintf("expected ',' or ')' but found %q", tokens[cursor].text),
				)}
			}
		}
	}
	cursor++

valuesParsed:
	if tokens[cursor].kind != tokenEnd {
		return nil, Diagnostics{errorDiagnostic(
			base+".values",
			fmt.Sprintf("unexpected token %q after the value list", tokens[cursor].text),
		)}
	}
	if len(values) == 0 {
		return nil, Diagnostics{errorDiagnostic(base+".values[0]", "at least one value is required")}
	}
	if diagnostics := validateLabelValues(base, values); diagnostics != nil {
		return nil, diagnostics
	}
	return newTerm(base, key, operator, values), nil
}

// parseExactValue parses the single value of an equals-family or gt/lt
// requirement.
func parseExactValue(base, key string, operator selection.Operator, operatorColumn int, tokens []token, cursor int) (*term, Diagnostics) {
	if tokens[cursor].kind != tokenIdentifier {
		return nil, Diagnostics{errorDiagnostic(
			base+".values[0]",
			fmt.Sprintf("expected a value after the operator at column %d but found %q", operatorColumn+1, tokens[cursor].text),
		)}
	}
	value := tokens[cursor].text
	cursor++

	if tokens[cursor].kind != tokenEnd {
		return nil, Diagnostics{errorDiagnostic(
			base+".values",
			fmt.Sprintf("unexpected token %q after the requirement value", tokens[cursor].text),
		)}
	}

	if reasons := validation.IsValidLabelValue(value); len(reasons) > 0 {
		return nil, Diagnostics{errorDiagnostic(base+".values[0]", strings.Join(reasons, "; "))}
	}
	if operator == selection.GreaterThan || operator == selection.LessThan {
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return nil, Diagnostics{errorDiagnostic(base+".values[0]", fmt.Sprintf("value for %q must be a 64-bit integer, got %q", operator, value))}
		}
	}
	return newTerm(base, key, operator, []string{value}), nil
}

func validateLabelValues(base string, values []string) Diagnostics {
	for index, value := range values {
		if reasons := validation.IsValidLabelValue(value); len(reasons) > 0 {
			return Diagnostics{errorDiagnostic(
				fmt.Sprintf("%s.values[%d]", base, index),
				strings.Join(reasons, "; "),
			)}
		}
	}
	return nil
}

// parseAnnotationRequirement parses a structured resource-annotation
// requirement. Annotation values are free-form, so only key grammar and
// operator arity are validated.
func parseAnnotationRequirement(requirement AnnotationRequirement, index int) (*term, Diagnostics) {
	base := fmt.Sprintf("%s[%d]", annotationsField, index)
	operator, ok := resolveAnnotationOperator(requirement.Operator)
	if !ok {
		return nil, Diagnostics{errorDiagnostic(
			base+".operator",
			fmt.Sprintf("unsupported operator %q; supported operators are: exists, doesnotexist, =, ==, !=, in, notin, >, <", requirement.Operator),
		)}
	}

	key := strings.TrimSpace(requirement.Key)
	if reasons := validation.IsQualifiedName(key); len(reasons) > 0 {
		return nil, Diagnostics{errorDiagnostic(base+".key", strings.Join(reasons, "; "))}
	}

	switch operator {
	case selection.Exists, selection.DoesNotExist:
		if len(requirement.Values) != 0 {
			return nil, Diagnostics{errorDiagnostic(base+".values", "values must be empty for exists and doesnotexist")}
		}
	case selection.In, selection.NotIn:
		if len(requirement.Values) == 0 {
			return nil, Diagnostics{errorDiagnostic(base+".values", "at least one value is required for in and notin")}
		}
	default:
		if len(requirement.Values) != 1 {
			return nil, Diagnostics{errorDiagnostic(base+".values", "exactly one value is required for this operator")}
		}
		if operator == selection.GreaterThan || operator == selection.LessThan {
			if _, err := strconv.ParseInt(requirement.Values[0], 10, 64); err != nil {
				return nil, Diagnostics{errorDiagnostic(base+".values[0]", fmt.Sprintf("value for %q must be a 64-bit integer, got %q", operator, requirement.Values[0]))}
			}
		}
	}

	values := append([]string(nil), requirement.Values...)
	return newTerm(base, key, operator, values), nil
}

// resolveAnnotationOperator maps the accepted, case-insensitive operator
// spellings onto canonical selection operators.
func resolveAnnotationOperator(raw string) (selection.Operator, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "exists":
		return selection.Exists, true
	case "doesnotexist", "!":
		return selection.DoesNotExist, true
	case "=", "==", "equals":
		return selection.Equals, true
	case "!=", "notequals":
		return selection.NotEquals, true
	case "in":
		return selection.In, true
	case "notin":
		return selection.NotIn, true
	case ">", "gt", "greaterthan":
		return selection.GreaterThan, true
	case "<", "lt", "lessthan":
		return selection.LessThan, true
	default:
		return "", false
	}
}

// valueProvenance pairs an allowed value with the requirement that introduced
// it, so conflict diagnostics can name both offending fields.
type valueProvenance struct {
	value string
	term  *term
}

// findConflicts detects unsatisfiable requirement combinations over a single
// requirement source (labels or annotations) and duplicate-requirement
// warnings. Requirements over different keys never conflict. Label and
// annotation requirements are analyzed independently.
func findConflicts(terms []*term) Diagnostics {
	diagnostics := Diagnostics{}
	if len(terms) < 2 {
		return diagnostics
	}

	order := []string{}
	grouped := map[string][]*term{}
	for _, requirement := range terms {
		if _, seen := grouped[requirement.key]; !seen {
			order = append(order, requirement.key)
		}
		grouped[requirement.key] = append(grouped[requirement.key], requirement)
	}

	for _, key := range order {
		diagnostics = append(diagnostics, findDuplicates(key, grouped[key])...)
		diagnostics = append(diagnostics, findKeyConflicts(key, grouped[key])...)
	}
	return diagnostics
}

// findDuplicates warns about semantically identical repeated requirements.
func findDuplicates(key string, terms []*term) Diagnostics {
	diagnostics := Diagnostics{}
	seen := map[string]*term{}
	for _, requirement := range terms {
		signature := duplicateSignature(requirement)
		if previous, ok := seen[signature]; ok {
			diagnostics = append(diagnostics, warningDiagnostic(
				requirement.field,
				fmt.Sprintf("duplicate requirement for key %q (already specified at %s)", key, previous.field),
			))
			continue
		}
		seen[signature] = requirement
	}
	return diagnostics
}

func duplicateSignature(requirement *term) string {
	return string(canonicalOperator(requirement.operator)) + "|" + strings.Join(requirement.valueList, ",")
}

// canonicalOperator folds equivalent spellings (== vs =) together for
// duplicate detection and rendering.
func canonicalOperator(operator selection.Operator) selection.Operator {
	if operator == selection.DoubleEquals {
		return selection.Equals
	}
	return operator
}

// findKeyConflicts detects requirements for the same key that can never hold
// simultaneously.
func findKeyConflicts(key string, terms []*term) Diagnostics {
	diagnostics := Diagnostics{}

	var (
		present    *term
		absent     *term
		equals     map[string]*term
		inSets     [][]valueProvenance
		forbidden  map[string]*term
		greatestGT *term
		smallestLT *term
	)

	for _, requirement := range terms {
		switch requirement.operator {
		case selection.DoesNotExist:
			absent = requirement
		case selection.Exists:
			present = firstRequirement(present, requirement)
		case selection.Equals, selection.DoubleEquals:
			if equals == nil {
				equals = map[string]*term{}
			}
			if _, ok := equals[requirement.valueList[0]]; !ok {
				equals[requirement.valueList[0]] = requirement
			}
			present = firstRequirement(present, requirement)
		case selection.NotEquals:
			if forbidden == nil {
				forbidden = map[string]*term{}
			}
			if _, ok := forbidden[requirement.valueList[0]]; !ok {
				forbidden[requirement.valueList[0]] = requirement
			}
		case selection.In:
			provenance := make([]valueProvenance, 0, len(requirement.valueList))
			for _, value := range requirement.valueList {
				provenance = append(provenance, valueProvenance{value: value, term: requirement})
			}
			inSets = append(inSets, provenance)
			present = firstRequirement(present, requirement)
		case selection.NotIn:
			if forbidden == nil {
				forbidden = map[string]*term{}
			}
			for _, value := range requirement.valueList {
				if _, ok := forbidden[value]; !ok {
					forbidden[value] = requirement
				}
			}
		case selection.GreaterThan:
			if greatestGT == nil || requirement.number > greatestGT.number {
				greatestGT = requirement
			}
			present = firstRequirement(present, requirement)
		case selection.LessThan:
			if smallestLT == nil || requirement.number < smallestLT.number {
				smallestLT = requirement
			}
			present = firstRequirement(present, requirement)
		}
	}

	if present != nil && absent != nil {
		diagnostics = append(diagnostics, conflictDiagnostic(
			absent, present,
			fmt.Sprintf("key %q must exist and not exist at the same time", key),
		))
	}

	// Equal requirements with distinct values contradict each other. Report
	// them in source order so the diagnostic field always points at the later
	// requirement and the message names the earlier one.
	var baselineEqual *term
	for _, requirement := range terms {
		if canonicalOperator(requirement.operator) != selection.Equals {
			continue
		}
		if baselineEqual == nil {
			baselineEqual = requirement
			continue
		}
		if requirement.valueList[0] != baselineEqual.valueList[0] {
			diagnostics = append(diagnostics, conflictDiagnostic(
				requirement, baselineEqual,
				fmt.Sprintf("key %q cannot equal %q and %q at the same time", key, baselineEqual.valueList[0], requirement.valueList[0]),
			))
		}
	}

	// allowed is the set of values that all equals/in requirements permit.
	var allowed map[string]valueProvenance
	if len(equals) > 0 {
		allowed = make(map[string]valueProvenance, len(equals))
		for value, requirement := range equals {
			allowed[value] = valueProvenance{value: value, term: requirement}
		}
	} else if len(inSets) > 0 {
		allowed = make(map[string]valueProvenance, len(inSets[0]))
		for _, provenance := range inSets[0] {
			allowed[provenance.value] = provenance
		}
	}

	for index := 1; index < len(inSets); index++ {
		next := map[string]valueProvenance{}
		for _, provenance := range inSets[index] {
			if existing, ok := allowed[provenance.value]; ok {
				next[provenance.value] = existing
			}
		}
		allowed = next
	}

	// Intersect the positive value domain (equals/in) with every negative
	// requirement (notequals/notin). The combination is only satisfiable when
	// at least one permitted value survives.
	if len(equals) > 0 || len(inSets) > 0 {
		remaining := map[string]valueProvenance{}
		for value, provenance := range allowed {
			if _, banned := forbidden[value]; !banned {
				remaining[value] = provenance
			}
		}
		if len(remaining) == 0 {
			diagnostics = append(diagnostics, emptyDomainConflict(key, terms, equals, inSets, forbidden))
		}
	}

	if greatestGT != nil && smallestLT != nil && greatestGT.number >= smallestLT.number {
		diagnostics = append(diagnostics, conflictDiagnostic(
			smallestLT, greatestGT,
			fmt.Sprintf("key %q must be greater than %d and less than %d at the same time", key, greatestGT.number, smallestLT.number),
		))
	}

	if len(equals) > 0 && (greatestGT != nil || smallestLT != nil) {
		for value, requiring := range equals {
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				continue
			}
			if greatestGT != nil && number <= greatestGT.number {
				diagnostics = append(diagnostics, conflictDiagnostic(
					requiring, greatestGT,
					fmt.Sprintf("key %q equals %d which is not greater than %d", key, number, greatestGT.number),
				))
			}
			if smallestLT != nil && number >= smallestLT.number {
				diagnostics = append(diagnostics, conflictDiagnostic(
					requiring, smallestLT,
					fmt.Sprintf("key %q equals %d which is not less than %d", key, number, smallestLT.number),
				))
			}
		}
	}

	return diagnostics
}

func firstRequirement(current, candidate *term) *term {
	if current == nil {
		return candidate
	}
	return current
}

// emptyDomainConflict builds the diagnostic for a positive value domain fully
// consumed by disjoint in sets or by negative requirements. It places the
// diagnostic on the requirement appearing later in the source so both
// offending fields are discoverable.
func emptyDomainConflict(key string, allTerms []*term, equals map[string]*term, inSets [][]valueProvenance, forbidden map[string]*term) Diagnostic {
	positiveCandidates := make([]*term, 0, len(equals))
	for _, requirement := range equals {
		positiveCandidates = append(positiveCandidates, requirement)
	}
	for _, set := range inSets {
		for _, provenance := range set {
			positiveCandidates = append(positiveCandidates, provenance.term)
		}
	}
	positive := latestTerm(allTerms, positiveCandidates)

	negativeCandidates := make([]*term, 0, len(forbidden))
	for _, requirement := range forbidden {
		negativeCandidates = append(negativeCandidates, requirement)
	}
	negative := latestTerm(allTerms, negativeCandidates)

	if negative == nil {
		earlier := earliestTerm(allTerms, positiveCandidates)
		return conflictDiagnostic(
			positive, earlier,
			fmt.Sprintf("the in requirements for key %q have no values in common", key),
		)
	}
	if termIndex(allTerms, negative) > termIndex(allTerms, positive) {
		return conflictDiagnostic(
			negative, positive,
			fmt.Sprintf("requirements for key %q leave no possible value: every allowed value is excluded", key),
		)
	}
	return conflictDiagnostic(
		positive, negative,
		fmt.Sprintf("requirements for key %q leave no possible value: every excluded value is required", key),
	)
}

func termIndex(allTerms []*term, target *term) int {
	for index, requirement := range allTerms {
		if requirement == target {
			return index
		}
	}
	return -1
}

func latestTerm(allTerms, candidates []*term) *term {
	var latest *term
	latestIndex := -1
	for _, candidate := range candidates {
		if index := termIndex(allTerms, candidate); index > latestIndex {
			latestIndex = index
			latest = candidate
		}
	}
	return latest
}

func earliestTerm(allTerms, candidates []*term) *term {
	var earliest *term
	earliestIndex := len(allTerms) + 1
	for _, candidate := range candidates {
		if index := termIndex(allTerms, candidate); index < earliestIndex {
			earliestIndex = index
			earliest = candidate
		}
	}
	return earliest
}

func conflictDiagnostic(later, earlier *term, message string) Diagnostic {
	return errorDiagnostic(
		later.field,
		fmt.Sprintf("conflict with %s: %s", earlier.field, message),
	)
}

// renderCanonical renders the compiled requirements in a stable, human-readable
// form. Label terms use Kubernetes label-selector syntax.
func renderCanonical(labelTerms, annotationTerms []*term) string {
	parts := []string{}
	for _, requirement := range labelTerms {
		parts = append(parts, renderLabelTerm(requirement))
	}
	for _, requirement := range annotationTerms {
		parts = append(parts, renderAnnotationTerm(requirement))
	}
	return strings.Join(parts, " && ")
}

func renderLabelTerm(requirement *term) string {
	key := requirement.key
	switch canonicalOperator(requirement.operator) {
	case selection.Exists:
		return key
	case selection.DoesNotExist:
		return "!" + key
	case selection.Equals:
		return key + "=" + requirement.valueList[0]
	case selection.NotEquals:
		return key + "!=" + requirement.valueList[0]
	case selection.In:
		return key + " in (" + strings.Join(requirement.valueList, ",") + ")"
	case selection.NotIn:
		return key + " notin (" + strings.Join(requirement.valueList, ",") + ")"
	case selection.GreaterThan:
		return key + ">" + requirement.valueList[0]
	case selection.LessThan:
		return key + "<" + requirement.valueList[0]
	default:
		return key
	}
}

func renderAnnotationTerm(requirement *term) string {
	key := "annotations[" + strconv.Quote(requirement.key) + "]"
	quoted := make([]string, 0, len(requirement.valueList))
	for _, value := range requirement.valueList {
		quoted = append(quoted, strconv.Quote(value))
	}
	switch canonicalOperator(requirement.operator) {
	case selection.Exists:
		return key + " exists"
	case selection.DoesNotExist:
		return key + " doesnotexist"
	case selection.Equals:
		return key + "=" + quoted[0]
	case selection.NotEquals:
		return key + "!=" + quoted[0]
	case selection.In:
		return key + " in (" + strings.Join(quoted, ",") + ")"
	case selection.NotIn:
		return key + " notin (" + strings.Join(quoted, ",") + ")"
	case selection.GreaterThan:
		return key + ">" + quoted[0]
	case selection.LessThan:
		return key + "<" + quoted[0]
	default:
		return key
	}
}
