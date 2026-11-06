// Package selector compiles the namespace portion of reload rules into
// immutable matching predicates.
//
// Compilation is a pure, side-effect free operation: it never contacts the
// Kubernetes API server, starts informers, or triggers workload updates. The
// compiler only inspects the supplied Spec and evaluates predicates against
// in-memory NamespaceView snapshots.
//
// A Compiler caches the compiled result per selector version. Recompiling the
// same version returns the cached predicate; changing Spec.Version atomically
// replaces the cache entry, so old predicates remain usable while new events
// observe the new one.
package selector

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/selection"
)

// DiagnosticSeverity classifies a compile-time diagnostic.
type DiagnosticSeverity int

const (
	// SeverityError prevents the selector from compiling into a predicate.
	SeverityError DiagnosticSeverity = iota
	// SeverityWarning reports a non-fatal issue such as a duplicate requirement.
	SeverityWarning
)

// Diagnostic reports a syntax problem or a semantic conflict found while
// compiling a reload-rule namespace selector. Field is a slash-free path that
// pinpoints the offending configuration element, for example
// "labelSelector.requirements[1].operator".
type Diagnostic struct {
	// Field pinpoints the offending configuration element.
	Field string
	// Severity classifies the diagnostic.
	Severity DiagnosticSeverity
	// Message describes the problem.
	Message string
}

// Error implements the error interface.
func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s: %s", d.Field, d.Message)
}

// Diagnostics is an ordered collection of Diagnostic values.
type Diagnostics []Diagnostic

// Error aggregates all diagnostics into a single error string.
func (d Diagnostics) Error() string {
	if len(d) == 0 {
		return ""
	}
	parts := make([]string, 0, len(d))
	for _, item := range d {
		parts = append(parts, item.Error())
	}
	return strings.Join(parts, "; ")
}

// HasErrors reports whether the collection contains an error diagnostic.
func (d Diagnostics) HasErrors() bool {
	for _, item := range d {
		if item.Severity == SeverityError {
			return true
		}
	}
	return false
}

// AnnotationRequirement expresses a requirement over a namespace annotation.
// Operator accepts one of: "exists", "doesnotexist", "=", "==", "!=", "in",
// "notin", ">", "lt" (case-insensitive aliases "equals", "notequals", "gt",
// "lt" are also accepted).
type AnnotationRequirement struct {
	// Key is the annotation key (a qualified Kubernetes name).
	Key string
	// Operator is the requirement operator.
	Operator string
	// Values holds the operand values; empty for exists/doesnotexist.
	Values []string
}

// Spec is the namespace-selector portion of a reload rule.
type Spec struct {
	// Version identifies this revision of the selector source (for example a
	// resourceVersion or a content hash). The compiled cache entry is valid
	// only for this exact version; any change invalidates the previous entry.
	Version string
	// LabelSelector is a Kubernetes label-selector expression, for example
	// "env=prod,tier!=canary". An empty string imposes no label requirements.
	LabelSelector string
	// ResourceAnnotations adds requirements over namespace annotations.
	ResourceAnnotations []AnnotationRequirement
}

// NamespaceView is the immutable snapshot of a namespace evaluated by a
// Predicate. Callers must not mutate the maps while a match is in progress.
type NamespaceView struct {
	// Name is the namespace name (informational, not used for matching).
	Name string
	// Labels are the namespace labels.
	Labels map[string]string
	// Annotations are the namespace annotations.
	Annotations map[string]string
}

// Predicate is an immutable, concurrency-safe namespace matcher. Instances are
// safe to call from multiple goroutines and never change after construction.
type Predicate interface {
	// Matches reports whether the view satisfies every compiled requirement.
	Matches(view NamespaceView) bool
	// Empty reports whether the predicate imposes no requirements at all.
	Empty() bool
	// Version returns the selector version this predicate was compiled from.
	Version() string
	// String renders the canonical form of the compiled requirements.
	String() string
}

// Compile parses and validates spec without caching. It performs no I/O and
// never accesses a cluster. When the spec contains syntax errors the returned
// Predicate is nil and the diagnostics carry field-located errors. Semantic
// conflicts that leave the predicate unsatisfiable are returned as error
// diagnostics while the predicate is still produced, allowing callers to
// inspect both. Non-fatal issues (for example duplicate requirements) are
// returned as warnings.
func Compile(spec Spec) (Predicate, Diagnostics) {
	return compile(spec)
}

// cachedEntry is an immutable compiler cache snapshot.
type cachedEntry struct {
	version     string
	predicate   Predicate
	diagnostics Diagnostics
}

// Compiler compiles namespace selectors and memoizes the result per selector
// version. The zero value is ready to use. A Compiler must not be copied after
// first use.
type Compiler struct {
	compileMu sync.Mutex
	entry     atomic.Pointer[cachedEntry]
}

// NewCompiler returns a ready-to-use Compiler.
func NewCompiler() *Compiler {
	return &Compiler{}
}

// Compile builds the immutable predicate for spec. The fast path is lock free:
// when the cached entry was compiled for spec.Version it is returned as-is.
// A version change takes the slow path, serializes concurrent compiles, and
// atomically swaps the cache entry. Compilation never contacts the cluster and
// never triggers updates.
func (c *Compiler) Compile(spec Spec) (Predicate, Diagnostics) {
	if cached := c.entry.Load(); cached != nil && cached.version == spec.Version {
		return cached.predicate, cached.diagnostics
	}

	c.compileMu.Lock()
	defer c.compileMu.Unlock()

	if cached := c.entry.Load(); cached != nil && cached.version == spec.Version {
		return cached.predicate, cached.diagnostics
	}

	predicate, diagnostics := compile(spec)
	c.entry.Store(&cachedEntry{
		version:     spec.Version,
		predicate:   predicate,
		diagnostics: diagnostics,
	})
	return predicate, diagnostics
}

// Invalidate drops the cached entry, if any. The next Compile call rebuilds
// the predicate even if the version is unchanged.
func (c *Compiler) Invalidate() {
	c.compileMu.Lock()
	defer c.compileMu.Unlock()
	c.entry.Store(nil)
}

// immutablePredicate is built once and thereafter only read, which makes
// concurrent Matches calls race free.
type immutablePredicate struct {
	version         string
	labelTerms      []*term
	annotationTerms []*term
	canonical       string
}

// Matches implements Predicate.
func (p *immutablePredicate) Matches(view NamespaceView) bool {
	for _, requirement := range p.labelTerms {
		if !requirement.matches(view.Labels) {
			return false
		}
	}
	for _, requirement := range p.annotationTerms {
		if !requirement.matches(view.Annotations) {
			return false
		}
	}
	return true
}

// Empty implements Predicate.
func (p *immutablePredicate) Empty() bool {
	return len(p.labelTerms) == 0 && len(p.annotationTerms) == 0
}

// Version implements Predicate.
func (p *immutablePredicate) Version() string {
	return p.version
}

// String implements Predicate.
func (p *immutablePredicate) String() string {
	return p.canonical
}

func compile(spec Spec) (Predicate, Diagnostics) {
	diagnostics := Diagnostics{}
	parseFailed := false

	labelTerms := []*term{}
	if strings.TrimSpace(spec.LabelSelector) != "" {
		for index, raw := range splitRequirements(spec.LabelSelector) {
			requirement, termDiagnostics := parseLabelRequirement(raw, index)
			diagnostics = append(diagnostics, termDiagnostics...)
			if termDiagnostics.HasErrors() {
				parseFailed = true
			}
			if requirement != nil {
				labelTerms = append(labelTerms, requirement)
			}
		}
	}

	annotationTerms := []*term{}
	for index, requirement := range spec.ResourceAnnotations {
		parsed, termDiagnostics := parseAnnotationRequirement(requirement, index)
		diagnostics = append(diagnostics, termDiagnostics...)
		if termDiagnostics.HasErrors() {
			parseFailed = true
		}
		if parsed != nil {
			annotationTerms = append(annotationTerms, parsed)
		}
	}

	// Syntax errors are fatal: no predicate is produced. Conflict analysis
	// over partial requirements would report misleading contradictions, so it
	// only runs once every requirement parsed cleanly. Semantic conflicts are
	// reported as error diagnostics but do not suppress the predicate, which
	// simply matches nothing for the contradictory key.
	if parseFailed {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, findConflicts(labelTerms)...)
	diagnostics = append(diagnostics, findConflicts(annotationTerms)...)

	predicate := &immutablePredicate{
		version:         spec.Version,
		labelTerms:      labelTerms,
		annotationTerms: annotationTerms,
		canonical:       renderCanonical(labelTerms, annotationTerms),
	}
	return predicate, diagnostics
}

// term is a single, fully validated requirement. values is a private, never
// mutated set; valueList holds the same values in sorted order for stable
// rendering and singleton access.
type term struct {
	field     string
	key       string
	operator  selection.Operator
	values    map[string]struct{}
	valueList []string
	number    int64
}

// matches mirrors k8s.io/apimachinery/pkg/labels.Requirement.Matches semantics:
// NotEquals and NotIn also match when the key is absent.
func (t *term) matches(source map[string]string) bool {
	value, found := source[t.key]
	switch t.operator {
	case selection.Exists:
		return found
	case selection.DoesNotExist:
		return !found
	case selection.Equals, selection.DoubleEquals:
		return found && value == t.valueList[0]
	case selection.NotEquals:
		return !found || value != t.valueList[0]
	case selection.In:
		if !found {
			return false
		}
		_, ok := t.values[value]
		return ok
	case selection.NotIn:
		if !found {
			return true
		}
		_, ok := t.values[value]
		return !ok
	case selection.GreaterThan:
		if !found {
			return false
		}
		actual, err := strconv.ParseInt(value, 10, 64)
		return err == nil && actual > t.number
	case selection.LessThan:
		if !found {
			return false
		}
		actual, err := strconv.ParseInt(value, 10, 64)
		return err == nil && actual < t.number
	default:
		return false
	}
}

func newTerm(fieldPath, key string, operator selection.Operator, values []string) *term {
	requirement := &term{
		field:    fieldPath,
		key:      key,
		operator: operator,
		values:   make(map[string]struct{}, len(values)),
	}
	for _, value := range values {
		requirement.values[value] = struct{}{}
	}
	requirement.valueList = append(requirement.valueList, values...)
	sort.Strings(requirement.valueList)
	if operator == selection.GreaterThan || operator == selection.LessThan {
		requirement.number, _ = strconv.ParseInt(values[0], 10, 64)
	}
	return requirement
}

func errorDiagnostic(fieldPath, message string) Diagnostic {
	return Diagnostic{Field: fieldPath, Severity: SeverityError, Message: message}
}

func warningDiagnostic(fieldPath, message string) Diagnostic {
	return Diagnostic{Field: fieldPath, Severity: SeverityWarning, Message: message}
}

// splitRequirements splits on top-level commas, ignoring commas nested inside
// the parenthesized value lists of in/notin requirements.
func splitRequirements(raw string) []string {
	requirements := []string{}
	depth := 0
	start := 0
	for index, ch := range raw {
		switch ch {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				requirements = append(requirements, raw[start:index])
				start = index + 1
			}
		}
	}
	requirements = append(requirements, raw[start:])
	return requirements
}

// supportedOperatorText lists the operators accepted in label expressions.
const supportedOperatorText = "=, ==, !=, >, <, in, notin"
