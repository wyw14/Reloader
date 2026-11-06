package migration

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Action describes what happened to a single field during a migration.
type Action string

const (
	// ActionCopied keeps the field as-is.
	ActionCopied Action = "copied"
	// ActionRenamed moves a field to a new key.
	ActionRenamed Action = "renamed"
	// ActionDefaulted fills an absent field with a rule-consistent default.
	ActionDefaulted Action = "defaulted"
	// ActionTransformed converts the field's shape or value semantics.
	ActionTransformed Action = "transformed"
	// ActionConflict records legacy and current fields disagreeing.
	ActionConflict Action = "conflict"
	// ActionPreservedUnknown moves an unrecognized field into the safe envelope.
	ActionPreservedUnknown Action = "preservedUnknown"
	// ActionRedacted records removal of Secret material.
	ActionRedacted Action = "redacted"
)

// FieldStep is one field-level migration action. It intentionally carries no
// field values so that plans are safe to log or persist.
type FieldStep struct {
	FromVersion string `json:"fromVersion"`
	ToVersion   string `json:"toVersion"`
	Path        string `json:"path"`
	Action      Action `json:"action"`
	Detail      string `json:"detail,omitempty"`
}

// Plan is the field-by-field migration report plus the internal rollback
// snapshot needed to undo a migration.
type Plan struct {
	// Kind is "decision" or "event".
	Kind string `json:"kind"`
	// SourceVersion is the version detected on the input.
	SourceVersion string `json:"sourceVersion"`
	// TargetVersion is the requested output version.
	TargetVersion string `json:"targetVersion"`
	// Steps lists every field action in execution order.
	Steps []FieldStep `json:"steps"`
	// ConflictPolicy records how disagreements were resolved.
	ConflictPolicy ConflictPolicy `json:"conflictPolicy"`

	// snapshot is the scrubbed input as it looked before any hop ran. It is
	// never serialized so plans can be shared without leaking data.
	snapshot map[string]any
}

// String renders the plan without the rollback snapshot.
func (p *Plan) String() string {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// Snapshot returns a copy of the scrubbed pre-migration object used for
// rollback. The boolean is false when no snapshot exists.
func (p *Plan) Snapshot() (map[string]any, bool) {
	if p == nil || p.snapshot == nil {
		return nil, false
	}
	return deepCopyMap(p.snapshot), true
}

// Sentinel errors. All messages reference field paths or versions, never values.
var (
	// ErrInvalidInput means the input could not be parsed as an object.
	ErrInvalidInput = errors.New("invalid migration input")
	// ErrInvalidField means a known field had an unexpected type or was missing.
	ErrInvalidField = errors.New("invalid migration field")
	// ErrUnknownVersion means a source or target version is not registered.
	ErrUnknownVersion = errors.New("unknown schema version")
	// ErrDowngrade means the target predates the source; use Rollback instead.
	ErrDowngrade = errors.New("cannot migrate to an older version")
	// ErrFieldConflict means legacy and current fields disagree and the fail
	// policy is in effect.
	ErrFieldConflict = errors.New("field conflict")
	// ErrNoRollback means the plan carries no rollback snapshot.
	ErrNoRollback = errors.New("no rollback snapshot")
)

// ConflictPolicy decides behavior when legacy and current fields disagree.
type ConflictPolicy string

const (
	// ConflictFail fails the migration without producing an object.
	ConflictFail ConflictPolicy = "fail"
	// ConflictPreferLegacy resolves disagreements using the legacy field.
	ConflictPreferLegacy ConflictPolicy = "preferLegacy"
	// ConflictPreferCurrent resolves disagreements using the current field.
	ConflictPreferCurrent ConflictPolicy = "preferCurrent"
)

type migrateConfig struct {
	policy ConflictPolicy
}

// Option customizes a migration run.
type Option func(*migrateConfig)

// WithConflictPolicy selects how legacy/current field conflicts are resolved.
func WithConflictPolicy(policy ConflictPolicy) Option {
	return func(cfg *migrateConfig) {
		cfg.policy = policy
	}
}

// fieldKind classifies known fields during unknown-field collection.
type fieldKind int

const (
	scalarField fieldKind = iota
	objectField
	listField
)

// fieldSpec describes one known field and where its nested schema lives.
type fieldSpec struct {
	kind   fieldKind
	nested string
}

// schemaSpec lists the known fields of one object shape.
type schemaSpec struct {
	fields map[string]fieldSpec
}

// hopFunc upgrades one version to the next without any cluster access.
type hopFunc func(in map[string]any, policy ConflictPolicy, steps *[]FieldStep) (map[string]any, error)

// family ties together ordered versions, schemas, hops and finalization.
type family struct {
	name     string
	ordered  []string
	schemas  map[string]schemaSpec
	hops     map[string]hopFunc
	finalize func(doc map[string]any, version string)
}

// Migrator performs version-compatible migrations. It is stateless and safe
// for concurrent use.
type Migrator struct{}

// New returns a ready-to-use migrator.
func New() *Migrator {
	return &Migrator{}
}

// MigrateDecision migrates a workload reload decision object to targetVersion.
// The input is never modified: on success a new object is returned, on failure
// nil is returned together with the partial plan and the error.
func (m *Migrator) MigrateDecision(raw any, targetVersion string, opts ...Option) (map[string]any, *Plan, error) {
	return m.run(raw, targetVersion, decisionFamily, opts)
}

// MigrateEvent migrates an inbound watch event to targetVersion.
func (m *Migrator) MigrateEvent(raw any, targetVersion string, opts ...Option) (map[string]any, *Plan, error) {
	return m.run(raw, targetVersion, eventFamily, opts)
}

// Rollback restores the exact scrubbed object that a migration started from.
// Secret payloads cannot be restored because they are removed before planning.
func (m *Migrator) Rollback(plan *Plan) (map[string]any, error) {
	snapshot, ok := plan.Snapshot()
	if !ok {
		return nil, ErrNoRollback
	}
	return snapshot, nil
}

func (m *Migrator) run(raw any, target string, fam family, opts []Option) (map[string]any, *Plan, error) {
	cfg := migrateConfig{policy: ConflictFail}
	for _, opt := range opts {
		opt(&cfg)
	}
	plan := &Plan{Kind: fam.name, TargetVersion: target, ConflictPolicy: cfg.policy}

	doc, err := parseObject(raw)
	if err != nil {
		return nil, plan, err
	}

	source, err := detectVersion(doc)
	if err != nil {
		plan.SourceVersion = "unknown"
		return nil, plan, err
	}
	plan.SourceVersion = source
	if err := validateVersions(fam, source, target); err != nil {
		return nil, plan, err
	}

	working := deepCopyMap(doc)

	steps := scrubSensitive(working, "", source, nil)
	// Snapshot the scrubbed original before any transformation so failure or
	// explicit rollback can always restore the pre-migration state.
	plan.snapshot = deepCopyMap(working)

	markers := extractMarkers(working)
	newlyFound := map[string]UnknownMarker{}
	collectUnknowns(working, fam.schemas[source], fam.schemas, source, "", newlyFound)
	mergeMarkers(markers, newlyFound)
	applyMarkers(working, markers)
	for _, path := range sortedMarkerPaths(newlyFound) {
		steps = append(steps, FieldStep{
			FromVersion: source,
			ToVersion:   source,
			Path:        path,
			Action:      ActionPreservedUnknown,
			Detail:      UnknownFieldNote,
		})
	}

	current := source
	for _, next := range versionsBetween(fam.ordered, source, target) {
		hop := fam.hops[current+"->"+next]
		if hop == nil {
			plan.Steps = steps
			return nil, plan, fmt.Errorf("%w: no path from %s to %s", ErrUnknownVersion, current, next)
		}
		out, hopErr := hop(working, cfg.policy, &steps)
		if hopErr != nil {
			plan.Steps = steps
			return nil, plan, hopErr
		}
		working = out
		current = next
	}

	if fam.finalize != nil {
		fam.finalize(working, target)
	}
	plan.Steps = steps
	return working, plan, nil
}

// validateVersions ensures both versions are registered and ordered forward.
func validateVersions(fam family, source, target string) error {
	sourceIndex := indexOf(fam.ordered, source)
	targetIndex := indexOf(fam.ordered, target)
	if sourceIndex < 0 {
		return fmt.Errorf("%w: source version %q", ErrUnknownVersion, source)
	}
	if targetIndex < 0 {
		return fmt.Errorf("%w: target version %q", ErrUnknownVersion, target)
	}
	if targetIndex < sourceIndex {
		return fmt.Errorf("%w: %q is older than %q; use Rollback", ErrDowngrade, target, source)
	}
	return nil
}

func indexOf(list []string, value string) int {
	for i, item := range list {
		if item == value {
			return i
		}
	}
	return -1
}

// versionsBetween returns the versions to visit after source up to target,
// which is how intermediate versions are skipped through chained hops.
func versionsBetween(ordered []string, source, target string) []string {
	start := indexOf(ordered, source)
	end := indexOf(ordered, target)
	if start < 0 || end < 0 || end <= start {
		return nil
	}
	return ordered[start+1 : end+1]
}

// detectVersion reads schemaVersion, then apiVersion, defaulting to v1.
func detectVersion(doc map[string]any) (string, error) {
	if raw, present := doc[fSchemaVersion]; present {
		version, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("%w: %s must be a string", ErrInvalidField, fSchemaVersion)
		}
		return version, nil
	}
	if raw, present := doc[fAPIVersion]; present {
		version, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("%w: %s must be a string", ErrInvalidField, fAPIVersion)
		}
		if stripped, ok := strings.CutPrefix(version, fAPIVersionGroup); ok {
			return stripped, nil
		}
	}
	return VersionV1, nil
}

// collectUnknowns walks a schema and moves unrecognized fields into the bucket.
func collectUnknowns(doc map[string]any, spec schemaSpec, schemas map[string]schemaSpec,
	version, prefix string, bucket map[string]UnknownMarker) {
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		path := joinPath(prefix, key)
		known, ok := spec.fields[key]
		if !ok {
			bucket[path] = newMarker(doc[key], version)
			delete(doc, key)
			continue
		}
		switch known.kind {
		case objectField:
			nested, ok := doc[key].(map[string]any)
			if !ok {
				continue
			}
			collectUnknowns(nested, schemas[known.nested], schemas, version, path, bucket)
		case listField:
			items, ok := doc[key].([]any)
			if !ok {
				continue
			}
			for index, item := range items {
				nested, ok := item.(map[string]any)
				if !ok {
					continue
				}
				collectUnknowns(nested, schemas[known.nested], schemas, version,
					indexPath(path, index), bucket)
			}
		}
	}
}

// extractMarkers pulls an existing unknownFields envelope out of the document.
func extractMarkers(doc map[string]any) map[string]UnknownMarker {
	markers := map[string]UnknownMarker{}
	raw, ok := doc[fUnknownFields]
	delete(doc, fUnknownFields)
	if !ok {
		return markers
	}
	existing, ok := raw.(map[string]any)
	if !ok {
		return markers
	}
	for _, path := range sortedMapKeys(existing) {
		switch typed := existing[path].(type) {
		case map[string]any:
			if preserved, _ := typed["preserved"].(bool); preserved {
				markers[path] = UnknownMarker{
					Value:             typed["value"],
					Preserved:         true,
					DetectedInVersion: asStringValue(typed["detectedInVersion"]),
					Note:              asStringValue(typed["note"]),
				}
				continue
			}
			markers[path] = newMarker(typed, "")
		default:
			markers[path] = newMarker(typed, "")
		}
	}
	return markers
}

// mergeMarkers keeps previously recorded markers and only adds new paths.
func mergeMarkers(existing map[string]UnknownMarker, found map[string]UnknownMarker) {
	for path, marker := range found {
		if _, already := existing[path]; already {
			continue
		}
		existing[path] = marker
	}
}

// applyMarkers writes the safe envelope back into the document.
func applyMarkers(doc map[string]any, markers map[string]UnknownMarker) {
	if len(markers) == 0 {
		return
	}
	envelope := make(map[string]any, len(markers))
	for _, path := range sortedMarkerPaths(markers) {
		envelope[path] = markerToMap(markers[path])
	}
	doc[fUnknownFields] = envelope
}

func sortedMarkerPaths(markers map[string]UnknownMarker) []string {
	paths := make([]string, 0, len(markers))
	for path := range markers {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func sortedMapKeys(doc map[string]any) []string {
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func asStringValue(value any) string {
	text, _ := value.(string)
	return text
}

// computeDecisionID derives a stable identifier from decision content so that
// migrating the same record repeatedly yields the same id.
func computeDecisionID(doc map[string]any) string {
	content := deepCopyMap(doc)
	delete(content, fSchemaVersion)
	delete(content, fDecisionID)
	b, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])[:12]
}

// parseObject accepts a generic map or JSON bytes/string and always returns a
// fresh, detached object.
func parseObject(raw any) (map[string]any, error) {
	switch typed := raw.(type) {
	case nil:
		return nil, fmt.Errorf("%w: object is nil", ErrInvalidInput)
	case map[string]any:
		return deepCopyMap(typed), nil
	case json.RawMessage:
		return decodeObject(typed)
	case []byte:
		return decodeObject(typed)
	case string:
		return decodeObject([]byte(typed))
	default:
		return nil, fmt.Errorf("%w: unsupported type %T", ErrInvalidInput, raw)
	}
}

func decodeObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	doc, ok := generic.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: top-level value must be an object", ErrInvalidInput)
	}
	return doc, nil
}

// deepCopy clones generic JSON values while preserving json.Number scalars.
func deepCopy(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return deepCopyMap(typed)
	case []any:
		clone := make([]any, len(typed))
		for index, item := range typed {
			clone[index] = deepCopy(item)
		}
		return clone
	default:
		return value
	}
}

// deepCopyMap clones a generic JSON object.
func deepCopyMap(original map[string]any) map[string]any {
	clone := make(map[string]any, len(original))
	for key, item := range original {
		clone[key] = deepCopy(item)
	}
	return clone
}

// stringField reads an optional string scalar.
func stringField(doc map[string]any, key string) (string, bool, error) {
	raw, present := doc[key]
	if !present {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("%w: %s must be a string", ErrInvalidField, key)
	}
	return value, true, nil
}

// requiredString reads a mandatory string scalar.
func requiredString(doc map[string]any, key string) (string, error) {
	value, present, err := stringField(doc, key)
	if err != nil {
		return "", err
	}
	if !present {
		return "", fmt.Errorf("%w: %s is required", ErrInvalidField, key)
	}
	return value, nil
}

// boolField reads an optional boolean scalar.
func boolField(doc map[string]any, key string) (bool, bool, error) {
	raw, present := doc[key]
	if !present {
		return false, false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, true, fmt.Errorf("%w: %s must be a boolean", ErrInvalidField, key)
	}
	return value, true, nil
}

// addStep appends a field step.
func addStep(from, to, path string, action Action, detail string, steps *[]FieldStep) {
	*steps = append(*steps, FieldStep{
		FromVersion: from,
		ToVersion:   to,
		Path:        path,
		Action:      action,
		Detail:      detail,
	})
}
