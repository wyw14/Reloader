package migration

import (
	"sort"
	"strconv"
	"strings"
)

// RedactionMarker replaces any value found under a Secret-bearing key. Decisions
// are only supposed to carry references and non-reversible hashes, so a payload
// key is always treated as accidental data and removed before anything is
// planned, migrated, logged, or returned.
const RedactionMarker = "***REDACTED***"

// UnknownFieldNote is the safe marker note attached to every preserved field.
// Typed consumers must ignore the unknownFields envelope entirely.
const UnknownFieldNote = "unrecognized field: preserved verbatim, marked safe, and ignored by Reloader"

// UnknownMarker is the safe envelope for fields that do not exist in a schema.
type UnknownMarker struct {
	// Value holds the original value, unless it was redacted, in which case it
	// contains RedactionMarker.
	Value any `json:"value"`
	// Preserved is the explicit safety flag: the field is retained but must
	// never influence reload behavior.
	Preserved bool `json:"preserved"`
	// DetectedInVersion records the schema version in which the field was first
	// seen as unknown.
	DetectedInVersion string `json:"detectedInVersion"`
	// Note explains why the field is safe to ignore.
	Note string `json:"note,omitempty"`
}

// sensitiveKeys lists key names (normalized to lowercase without separators)
// that carry Secret material in Kubernetes-style objects. Reference keys such
// as secretName or secretRef are intentionally absent.
var sensitiveKeys = map[string]struct{}{
	"data":        {},
	"binarydata":  {},
	"stringdata":  {},
	"token":       {},
	"bearertoken": {},
	"password":    {},
	"passwd":      {},
	"secretvalue": {},
	"accesskey":   {},
	"secretkey":   {},
	"apikey":      {},
	"credentials": {},
	"credential":  {},
}

func isSensitiveKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
	_, ok := sensitiveKeys[normalized]
	return ok
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func indexPath(prefix string, index int) string {
	return prefix + "[" + strconv.Itoa(index) + "]"
}

// scrubSensitive walks the value in place replacing Secret-bearing values and
// recording one redaction step per removed path. It never records values.
func scrubSensitive(value any, prefix, version string, steps []FieldStep) []FieldStep {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			path := joinPath(prefix, key)
			if isSensitiveKey(key) {
				typed[key] = RedactionMarker
				steps = append(steps, FieldStep{
					FromVersion: version,
					ToVersion:   version,
					Path:        path,
					Action:      ActionRedacted,
					Detail:      "Secret-bearing key removed before migration",
				})
				continue
			}
			steps = scrubSensitive(typed[key], path, version, steps)
		}
	case []any:
		for index, element := range typed {
			steps = scrubSensitive(element, indexPath(prefix, index), version, steps)
		}
	}
	return steps
}

// newMarker wraps an unknown value with the safe preservation marker.
func newMarker(value any, version string) UnknownMarker {
	return UnknownMarker{
		Value:             value,
		Preserved:         true,
		DetectedInVersion: version,
		Note:              UnknownFieldNote,
	}
}

// markerToMap renders a marker for storage inside a generic object.
func markerToMap(marker UnknownMarker) map[string]any {
	return map[string]any{
		"value":             marker.Value,
		"preserved":         marker.Preserved,
		"detectedInVersion": marker.DetectedInVersion,
		"note":              marker.Note,
	}
}
