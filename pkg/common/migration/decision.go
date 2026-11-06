// Package migration provides version-compatible migration for workload reload
// decision records and the watch events that produced them.
//
// The migrator is a pure, in-memory component: it only transforms objects and
// events handed to it by the caller. It never contacts the Kubernetes API,
// never mutates its input, and never emits Secret payload values in migrated
// objects, migration plans, or errors.
package migration

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/stakater/Reloader/internal/pkg/constants"
)

// Schema versions understood by the migrator, ordered oldest to newest.
const (
	VersionV1 = "v1"
	VersionV2 = "v2"
	VersionV3 = "v3"

	// CurrentDecisionVersion is the schema version produced for decisions today.
	CurrentDecisionVersion = VersionV3
	// CurrentEventVersion is the schema version produced for watch events today.
	CurrentEventVersion = VersionV3
)

// Watch event types. Legacy decision records could only be produced by update
// events because --reload-on-create and --reload-on-delete default to false.
const (
	EventCreate = "CREATE"
	EventUpdate = "UPDATE"
	EventDelete = "DELETE"

	// DefaultEventType is the event type applied to records that predate events.
	DefaultEventType = EventUpdate
	// DefaultWorkloadKind is used when a legacy record did not record its kind.
	DefaultWorkloadKind = "Deployment"
)

// JSON field names shared by the versioned schemas.
const (
	fSchemaVersion = "schemaVersion"
	fAPIVersion    = "apiVersion"
	fUnknownFields = "unknownFields"

	fType            = "type"
	fKind            = "kind"
	fName            = "name"
	fNamespace       = "namespace"
	fResourceVersion = "resourceVersion"
	fUID             = "uid"
	fReason          = "reason"
	fDecidedAt       = "decidedAt"
	fStrategy        = "strategy"
	fHash            = "hash"
	fContainerRefs   = "containerRefs"
	fObservedAt      = "observedAt"
	fSources         = "sources"
	fWorkload        = "workload"
	fResource        = "resource"
	fEvent           = "event"
	fDecision        = "decision"
	fDecisionID      = "decisionId"
	fShouldReload    = "shouldReload"
	fAutoReload      = "autoReload"
	fWorkloadKind    = "workloadKind"
	fV1Configmap     = "configmap"
	fV1Secret        = "secret"
	fV1Auto          = "auto"
	fV1Reload        = "reload"
	fEventObject     = "object"
	fEventMetadata   = "metadata"
	fAPIVersionGroup = "reloader.stakater.com/"
)

// WorkloadRef identifies the workload a decision applies to.
type WorkloadRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ResourceRef identifies the ConfigMap, Secret or SecretProviderClass referenced
// by a decision. It carries references only, never payload values.
type ResourceRef struct {
	APIVersion      string `json:"apiVersion,omitempty"`
	Kind            string `json:"kind,omitempty"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// Event is the watch event that produced a decision.
type Event struct {
	Type     string      `json:"type"`
	Resource ResourceRef `json:"resource,omitempty"`
}

// Source describes a single resource that contributed to a reload decision.
// Hash is the non-reversible content digest already used by Reloader; Secret
// values are never present.
type Source struct {
	Type          string   `json:"type"`
	Name          string   `json:"name"`
	Namespace     string   `json:"namespace,omitempty"`
	Hash          string   `json:"hash,omitempty"`
	ContainerRefs []string `json:"containerRefs,omitempty"`
	ObservedAt    int64    `json:"observedAt,omitempty"`
}

// Outcome is the reload decision itself.
type Outcome struct {
	ShouldReload bool   `json:"shouldReload"`
	AutoReload   bool   `json:"autoReload"`
	Strategy     string `json:"strategy"`
	Reason       string `json:"reason,omitempty"`
}

// Decision is the current (v3) workload reload decision record.
type Decision struct {
	SchemaVersion string                   `json:"schemaVersion"`
	DecisionID    string                   `json:"decisionId"`
	Workload      WorkloadRef              `json:"workload"`
	Event         Event                    `json:"event,omitempty"`
	Sources       []Source                 `json:"sources"`
	Outcome       Outcome                  `json:"decision"`
	DecidedAt     int64                    `json:"decidedAt,omitempty"`
	UnknownFields map[string]UnknownMarker `json:"unknownFields,omitempty"`
}

// DecodeV3 parses a migrated object into the current typed decision model.
func DecodeV3(raw any) (*Decision, error) {
	doc, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	var decision Decision
	if err := reencode(doc, &decision); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidField, err)
	}
	return &decision, nil
}

// ToMap converts the typed decision back into its generic object form.
func (d *Decision) ToMap() (map[string]any, error) {
	var doc map[string]any
	if err := reencode(d, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// String renders a decision for logs without unknown-field values, which are
// preserved for forward compatibility but must never be trusted or printed.
func (d Decision) String() string {
	safe := d
	safe.UnknownFields = nil
	keys := make([]string, 0, len(d.UnknownFields))
	for key := range d.UnknownFields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	view := struct {
		Decision
		UnknownFieldKeys []string `json:"unknownFieldKeys,omitempty"`
	}{
		Decision:         safe,
		UnknownFieldKeys: keys,
	}
	b, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// reencode round-trips a value through JSON while keeping object/map shapes.
func reencode(in any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// decisionSchemaV1 is the implicit legacy schema: a flat record with a single
// configmap and/or secret trigger. Absence of schemaVersion also means v1.
// Forward-ported v2 fields (sources/shouldReload/autoReload) are tolerated as
// known fields so mixed-schema documents can be conflict-checked.
var decisionSchemaV1 = schemaSpec{fields: map[string]fieldSpec{
	fSchemaVersion: {kind: scalarField},
	fAPIVersion:    {kind: scalarField},
	fWorkload:      {kind: scalarField},
	fWorkloadKind:  {kind: scalarField},
	fNamespace:     {kind: scalarField},
	fV1Configmap:   {kind: scalarField},
	fV1Secret:      {kind: scalarField},
	fV1Auto:        {kind: scalarField},
	fV1Reload:      {kind: scalarField},
	fStrategy:      {kind: scalarField},
	fReason:        {kind: scalarField},
	fDecidedAt:     {kind: scalarField},
	fSources:       {kind: listField, nested: schemaSource},
	fShouldReload:  {kind: scalarField},
	fAutoReload:    {kind: scalarField},
	fUnknownFields: {kind: scalarField},
}}

// decisionSchemaV2 introduced a structured workload reference, typed sources
// and the explicit reload strategy.
var decisionSchemaV2 = schemaSpec{fields: map[string]fieldSpec{
	fSchemaVersion: {kind: scalarField},
	fAPIVersion:    {kind: scalarField},
	fWorkload:      {kind: objectField, nested: schemaWorkloadRef},
	fSources:       {kind: listField, nested: schemaSource},
	fShouldReload:  {kind: scalarField},
	fAutoReload:    {kind: scalarField},
	fStrategy:      {kind: scalarField},
	fReason:        {kind: scalarField},
	fDecidedAt:     {kind: scalarField},
	fUnknownFields: {kind: scalarField},
}}

// decisionSchemaV3 nests the outcome under "decision", adds the originating
// event envelope and a deterministic decision id.
var decisionSchemaV3 = schemaSpec{fields: map[string]fieldSpec{
	fSchemaVersion: {kind: scalarField},
	fAPIVersion:    {kind: scalarField},
	fDecisionID:    {kind: scalarField},
	fWorkload:      {kind: objectField, nested: schemaWorkloadRef},
	fEvent:         {kind: objectField, nested: schemaEvent},
	fSources:       {kind: listField, nested: schemaSource},
	fDecision:      {kind: objectField, nested: schemaOutcome},
	fDecidedAt:     {kind: scalarField},
	fUnknownFields: {kind: scalarField},
}}

var workloadRefSchema = schemaSpec{fields: map[string]fieldSpec{
	fKind:      {kind: scalarField},
	fNamespace: {kind: scalarField},
	fName:      {kind: scalarField},
}}

var sourceSchema = schemaSpec{fields: map[string]fieldSpec{
	fType:          {kind: scalarField},
	fName:          {kind: scalarField},
	fNamespace:     {kind: scalarField},
	fHash:          {kind: scalarField},
	fContainerRefs: {kind: scalarField},
	fObservedAt:    {kind: scalarField},
}}

var eventSchema = schemaSpec{fields: map[string]fieldSpec{
	fType:     {kind: scalarField},
	fResource: {kind: objectField, nested: schemaResourceRef},
}}

var resourceRefSchema = schemaSpec{fields: map[string]fieldSpec{
	fAPIVersion:      {kind: scalarField},
	fKind:            {kind: scalarField},
	fNamespace:       {kind: scalarField},
	fName:            {kind: scalarField},
	fResourceVersion: {kind: scalarField},
}}

var outcomeSchema = schemaSpec{fields: map[string]fieldSpec{
	fShouldReload: {kind: scalarField},
	fAutoReload:   {kind: scalarField},
	fStrategy:     {kind: scalarField},
	fReason:       {kind: scalarField},
}}

// decisionSchemas collects every object schema reached from a decision,
// including nested objects shared with the event family.
var decisionSchemas = map[string]schemaSpec{
	VersionV1:         decisionSchemaV1,
	VersionV2:         decisionSchemaV2,
	VersionV3:         decisionSchemaV3,
	schemaWorkloadRef: workloadRefSchema,
	schemaSource:      sourceSchema,
	schemaEvent:       eventSchema,
	schemaResourceRef: resourceRefSchema,
	schemaOutcome:     outcomeSchema,
}

const (
	schemaWorkloadRef = "workloadRef"
	schemaSource      = "source"
	schemaEvent       = "event"
	schemaResourceRef = "resourceRef"
	schemaOutcome     = "outcome"
)

// decisionFamily describes the ordered version chain and per-hop upgrades.
var decisionFamily = family{
	name:    "decision",
	ordered: []string{VersionV1, VersionV2, VersionV3},
	schemas: decisionSchemas,
	hops: map[string]hopFunc{
		VersionV1 + "->" + VersionV2: hopDecisionV1ToV2,
		VersionV2 + "->" + VersionV3: hopDecisionV2ToV3,
	},
	finalize: finalizeDecision,
}

// finalizeDecision stamps the target version and, for v3, fills a stable
// decision id. It only adds values that are absent, so re-running on an
// already-finalized record changes nothing.
func finalizeDecision(doc map[string]any, version string) {
	doc[fSchemaVersion] = version
	if version == VersionV3 {
		if _, ok := doc[fDecisionID]; !ok {
			doc[fDecisionID] = computeDecisionID(doc)
		}
	}
}

// hopDecisionV1ToV2 flattens the legacy record into structured fields.
func hopDecisionV1ToV2(in map[string]any, policy ConflictPolicy, steps *[]FieldStep) (map[string]any, error) {
	out := map[string]any{fSchemaVersion: VersionV2}

	workload, err := buildV2Workload(in, steps)
	if err != nil {
		return nil, err
	}
	out[fWorkload] = workload

	sources, err := resolveV2Sources(in, policy, steps)
	if err != nil {
		return nil, err
	}
	out[fSources] = sources

	if err := copyBoolPair(in, out, fV1Reload, fShouldReload, VersionV1, VersionV2, policy, steps); err != nil {
		return nil, err
	}
	if err := copyBoolPair(in, out, fV1Auto, fAutoReload, VersionV1, VersionV2, policy, steps); err != nil {
		return nil, err
	}
	if err := copyStrategyField(in, out, fStrategy, VersionV1, VersionV2, steps); err != nil {
		return nil, err
	}
	copyScalar(in, out, fReason, VersionV1, VersionV2, "", steps)
	copyScalar(in, out, fDecidedAt, VersionV1, VersionV2, "", steps)
	carryUnknownFields(in, out)

	return out, nil
}

// hopDecisionV2ToV3 groups the decision booleans/strategy under "decision" and
// synthesizes the update-only event envelope.
func hopDecisionV2ToV3(in map[string]any, _ ConflictPolicy, steps *[]FieldStep) (map[string]any, error) {
	out := map[string]any{fSchemaVersion: VersionV3}

	if raw, ok := in[fWorkload]; ok {
		out[fWorkload] = raw
		addStep(VersionV2, VersionV3, fWorkload, ActionCopied, "", steps)
	}

	sources, err := normalizeV2Sources(in[fSources])
	if err != nil {
		return nil, err
	}
	out[fSources] = toAnySourceList(sources)
	addStep(VersionV2, VersionV3, fSources, ActionCopied, "", steps)

	outcome, err := buildV3Outcome(in, steps)
	if err != nil {
		return nil, err
	}
	out[fDecision] = outcome

	out[fEvent] = map[string]any{fType: DefaultEventType}
	addStep(VersionV2, VersionV3, fEvent+"."+fType, ActionDefaulted,
		"legacy decisions were produced by update events (reload-on-create and reload-on-delete default off)",
		steps)

	copyScalar(in, out, fDecidedAt, VersionV2, VersionV3, "", steps)
	carryUnknownFields(in, out)

	return out, nil
}

// buildV2Workload converts the flat workload/namespace/workloadKind fields.
func buildV2Workload(in map[string]any, steps *[]FieldStep) (map[string]any, error) {
	name, err := requiredString(in, fWorkload)
	if err != nil {
		return nil, err
	}
	namespace, _, err := stringField(in, fNamespace)
	if err != nil {
		return nil, err
	}
	kind, present, err := stringField(in, fWorkloadKind)
	if err != nil {
		return nil, err
	}
	if !present {
		kind = DefaultWorkloadKind
		addStep(VersionV1, VersionV2, fWorkload+"."+fKind, ActionDefaulted,
			"legacy records without an explicit kind default to Deployment", steps)
	} else {
		addStep(VersionV1, VersionV2, fWorkloadKind, ActionCopied, "", steps)
	}

	addStep(VersionV1, VersionV2, fWorkload, ActionRenamed,
		"moved into structured workload reference", steps)
	addStep(VersionV1, VersionV2, fNamespace, ActionCopied, "", steps)

	return map[string]any{fKind: kind, fNamespace: namespace, fName: name}, nil
}

// resolveV2Sources turns configmap/secret triggers into typed sources, while
// detecting conflicts with forward-ported sources already present on a v1 doc.
func resolveV2Sources(in map[string]any, policy ConflictPolicy, steps *[]FieldStep) ([]any, error) {
	legacy := legacySources(in, steps)

	typedRaw, typedPresent := in[fSources]
	var typed []map[string]any
	if typedPresent {
		var err error
		typed, err = normalizeV2Sources(typedRaw)
		if err != nil {
			return nil, err
		}
	}

	switch {
	case len(legacy) > 0 && len(typed) > 0 && !sameSourceSets(legacy, typed):
		addStep(VersionV1, VersionV2, fSources, ActionConflict,
			"legacy configmap/secret fields disagree with typed sources", steps)
		switch policy {
		case ConflictPreferCurrent:
			addStep(VersionV1, VersionV2, fSources, ActionTransformed, "resolved using typed sources", steps)
			return toAnySourceList(typed), nil
		case ConflictPreferLegacy:
			addStep(VersionV1, VersionV2, fSources, ActionTransformed, "resolved using legacy fields", steps)
			return toAnySourceList(legacy), nil
		default:
			return nil, fmt.Errorf("%w: legacy triggers and typed %s disagree", ErrFieldConflict, fSources)
		}
	case len(typed) > 0:
		return toAnySourceList(typed), nil
	default:
		return toAnySourceList(legacy), nil
	}
}

// legacySources builds typed sources from the flat configmap/secret fields.
func legacySources(in map[string]any, steps *[]FieldStep) []map[string]any {
	namespace, _, _ := stringField(in, fNamespace)
	sources := make([]map[string]any, 0, 2)
	build := func(key, sourceType string) {
		name, present, err := stringField(in, key)
		if err != nil || !present || name == "" {
			return
		}
		sources = append(sources, map[string]any{
			fType:      sourceType,
			fName:      name,
			fNamespace: namespace,
		})
		addStep(VersionV1, VersionV2, key, ActionTransformed,
			"converted legacy trigger into typed source ("+sourceType+")", steps)
	}
	build(fV1Configmap, constants.ConfigmapEnvVarPostfix)
	build(fV1Secret, constants.SecretEnvVarPostfix)
	return sources
}

// sameSourceSets compares sources by type/name regardless of ordering.
func sameSourceSets(a, b []map[string]any) bool {
	keySet := func(list []map[string]any) map[string]struct{} {
		set := make(map[string]struct{}, len(list))
		for _, item := range list {
			set[fmt.Sprintf("%s|%s", item[fType], item[fName])] = struct{}{}
		}
		return set
	}
	left, right := keySet(a), keySet(b)
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}
	return true
}

func toAnySourceList(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, item := range in {
		out = append(out, item)
	}
	return out
}

// normalizeV2Sources validates a typed sources array element by element.
func normalizeV2Sources(raw any) ([]map[string]any, error) {
	if raw == nil {
		return []map[string]any{}, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be an array", ErrInvalidField, fSources)
	}
	out := make([]map[string]any, 0, len(list))
	for index, element := range list {
		item, ok := element.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: %s[%d] must be an object", ErrInvalidField, fSources, index)
		}
		if _, present, err := stringField(item, fType); err != nil || !present {
			return nil, fmt.Errorf("%w: %s[%d].%s is required", ErrInvalidField, fSources, index, fType)
		}
		if _, present, err := stringField(item, fName); err != nil || !present {
			return nil, fmt.Errorf("%w: %s[%d].%s is required", ErrInvalidField, fSources, index, fName)
		}
		out = append(out, item)
	}
	return out, nil
}

// copyBoolPair moves a legacy boolean into its renamed field, applying the
// conflict policy when both forms are present on a mixed-schema document.
func copyBoolPair(in, out map[string]any, legacyKey, currentKey, from, to string,
	policy ConflictPolicy, steps *[]FieldStep) error {
	legacyValue, legacyPresent, err := boolField(in, legacyKey)
	if err != nil {
		return err
	}
	currentValue, currentPresent, err := boolField(in, currentKey)
	if err != nil {
		return err
	}

	switch {
	case legacyPresent && currentPresent && legacyValue != currentValue:
		addStep(from, to, legacyKey, ActionConflict,
			"legacy and renamed boolean fields disagree", steps)
		switch policy {
		case ConflictPreferCurrent:
			out[currentKey] = currentValue
			addStep(from, to, currentKey, ActionTransformed, "resolved using renamed field", steps)
		case ConflictPreferLegacy:
			out[currentKey] = legacyValue
			addStep(from, to, currentKey, ActionTransformed, "resolved using legacy field", steps)
		default:
			return fmt.Errorf("%w: %s and %s disagree", ErrFieldConflict, legacyKey, currentKey)
		}
	case legacyPresent:
		out[currentKey] = legacyValue
		addStep(from, to, legacyKey, ActionRenamed, "renamed to "+currentKey, steps)
	case currentPresent:
		out[currentKey] = currentValue
		addStep(from, to, currentKey, ActionCopied, "", steps)
	}
	return nil
}

// buildV3Outcome nests the v2 flat decision fields under "decision".
func buildV3Outcome(in map[string]any, steps *[]FieldStep) (map[string]any, error) {
	outcome := map[string]any{}

	if err := nestBool(in, outcome, fShouldReload, steps); err != nil {
		return nil, err
	}
	if err := nestBool(in, outcome, fAutoReload, steps); err != nil {
		return nil, err
	}
	if err := copyStrategyField(in, outcome, fDecision+"."+fStrategy, VersionV2, VersionV3, steps); err != nil {
		return nil, err
	}
	copyScalar(in, outcome, fReason, VersionV2, VersionV3,
		"moved into decision outcome", steps)

	return outcome, nil
}

// nestBool copies a v2 boolean into the nested outcome, defaulting false.
func nestBool(in, outcome map[string]any, key string, steps *[]FieldStep) error {
	value, present, err := boolField(in, key)
	if err != nil {
		return err
	}
	if !present {
		value = false
		addStep(VersionV2, VersionV3, fDecision+"."+key, ActionDefaulted,
			"missing boolean keeps the zero-value default (false)", steps)
	} else {
		addStep(VersionV2, VersionV3, key, ActionTransformed, "moved into decision outcome", steps)
	}
	outcome[key] = value
	return nil
}

// copyStrategyField moves the reload strategy, defaulting to env-vars which is
// the existing --reload-strategy default (options.ReloadStrategy).
func copyStrategyField(in, out map[string]any, targetPath, from, to string, steps *[]FieldStep) error {
	value, present, err := stringField(in, fStrategy)
	if err != nil {
		return err
	}
	if !present {
		value = constants.EnvVarsReloadStrategy
		addStep(from, to, targetPath, ActionDefaulted,
			"matches the existing --reload-strategy default (env-vars)", steps)
	} else {
		addStep(from, to, targetPath, ActionCopied, "", steps)
	}
	out[fStrategy] = value
	return nil
}

// copyScalar carries a scalar field verbatim when present.
func copyScalar(in, out map[string]any, key, from, to, detail string, steps *[]FieldStep) {
	value, ok := in[key]
	if !ok {
		return
	}
	out[key] = value
	addStep(from, to, key, ActionCopied, detail, steps)
}

// carryUnknownFields preserves the safe unknown-field marker envelope.
func carryUnknownFields(in, out map[string]any) {
	if value, ok := in[fUnknownFields]; ok {
		out[fUnknownFields] = deepCopy(value)
	}
}
