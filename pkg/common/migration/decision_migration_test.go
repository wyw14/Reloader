package migration

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stakater/Reloader/internal/pkg/constants"
)

func v1Decision() map[string]any {
	return map[string]any{
		"workload":     "checkout",
		"workloadKind": "Deployment",
		"namespace":    "shop",
		"configmap":    "app-config",
		"secret":       "app-secret",
		"reload":       true,
		"auto":         false,
		"reason":       "named annotation matched",
		"decidedAt":    1700000000,
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	require.NoError(t, err)
	return string(b)
}

func findStep(steps []FieldStep, path string, action Action) (FieldStep, bool) {
	for _, step := range steps {
		if step.Path == path && step.Action == action {
			return step, true
		}
	}
	return FieldStep{}, false
}

func hasStep(steps []FieldStep, path string, action Action) bool {
	_, ok := findStep(steps, path, action)
	return ok
}

func TestMigrateDecision_V1ToV2_DefaultsMatchExistingRules(t *testing.T) {
	migrator := New()
	out, plan, err := migrator.MigrateDecision(v1Decision(), VersionV2)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, VersionV1, plan.SourceVersion)
	assert.Equal(t, VersionV2, plan.TargetVersion)
	assert.Equal(t, VersionV2, out[fSchemaVersion])

	workload := out[fWorkload].(map[string]any)
	assert.Equal(t, "checkout", workload[fName])
	assert.Equal(t, "shop", workload[fNamespace])
	assert.Equal(t, "Deployment", workload[fKind])

	sources := out[fSources].([]any)
	require.Len(t, sources, 2)
	first := sources[0].(map[string]any)
	second := sources[1].(map[string]any)
	assert.Equal(t, constants.ConfigmapEnvVarPostfix, first[fType])
	assert.Equal(t, "app-config", first[fName])
	assert.Equal(t, "shop", first[fNamespace])
	assert.Equal(t, constants.SecretEnvVarPostfix, second[fType])

	assert.Equal(t, true, out[fShouldReload])
	assert.Equal(t, false, out[fAutoReload])
	assert.Equal(t, "named annotation matched", out[fReason])
	assert.Equal(t, 1700000000, out[fDecidedAt])

	// Strategy defaults to the existing --reload-strategy default (env-vars).
	assert.Equal(t, constants.EnvVarsReloadStrategy, out[fStrategy])
	assert.True(t, hasStep(plan.Steps, fStrategy, ActionDefaulted))
}

func TestMigrateDecision_V1ToV3_SkipsIntermediateVersions(t *testing.T) {
	migrator := New()
	out, plan, err := migrator.MigrateDecision(v1Decision(), VersionV3)
	require.NoError(t, err)

	assert.Equal(t, VersionV3, out[fSchemaVersion])

	// The plan contains hops from both v1->v2 and v2->v3.
	hopVersions := map[string]struct{}{}
	for _, step := range plan.Steps {
		hopVersions[step.FromVersion+"->"+step.ToVersion] = struct{}{}
	}
	assert.Contains(t, hopVersions, VersionV1+"->"+VersionV2)
	assert.Contains(t, hopVersions, VersionV2+"->"+VersionV3)

	outcome := out[fDecision].(map[string]any)
	assert.Equal(t, true, outcome[fShouldReload])
	assert.Equal(t, false, outcome[fAutoReload])
	assert.Equal(t, constants.EnvVarsReloadStrategy, outcome[fStrategy])

	event := out[fEvent].(map[string]any)
	assert.Equal(t, EventUpdate, event[fType])
	assert.True(t, hasStep(plan.Steps, fEvent+"."+fType, ActionDefaulted))

	decisionID, ok := out[fDecisionID].(string)
	require.True(t, ok)
	assert.Len(t, decisionID, 12)

	// Sources made it through both hops.
	sources := out[fSources].([]any)
	require.Len(t, sources, 2)

	decision, err := DecodeV3(out)
	require.NoError(t, err)
	assert.Equal(t, "checkout", decision.Workload.Name)
	assert.Equal(t, EventUpdate, decision.Event.Type)
	assert.Len(t, decision.Sources, 2)
	assert.True(t, decision.Outcome.ShouldReload)
	assert.Equal(t, constants.EnvVarsReloadStrategy, decision.Outcome.Strategy)
}

func TestMigrateDecision_DefaultKindWhenAbsent(t *testing.T) {
	migrator := New()
	input := map[string]any{
		"workload":  "api",
		"namespace": "team-a",
	}
	out, plan, err := migrator.MigrateDecision(input, VersionV2)
	require.NoError(t, err)

	workload := out[fWorkload].(map[string]any)
	assert.Equal(t, DefaultWorkloadKind, workload[fKind])
	assert.True(t, hasStep(plan.Steps, fWorkload+"."+fKind, ActionDefaulted))
}

func TestMigrateDecision_V2ToV3_Direct(t *testing.T) {
	migrator := New()
	input := map[string]any{
		fSchemaVersion: VersionV2,
		fWorkload: map[string]any{
			fKind: "StatefulSet", fNamespace: "db", fName: "postgres",
		},
		fSources: []any{
			map[string]any{fType: constants.SecretEnvVarPostfix, fName: "pg-creds", fHash: "abc123"},
		},
		fShouldReload: true,
		fAutoReload:   true,
		fStrategy:     constants.AnnotationsReloadStrategy,
	}
	out, _, err := migrator.MigrateDecision(input, VersionV3)
	require.NoError(t, err)

	outcome := out[fDecision].(map[string]any)
	assert.Equal(t, constants.AnnotationsReloadStrategy, outcome[fStrategy])
	assert.Equal(t, true, outcome[fAutoReload])
	source := out[fSources].([]any)[0].(map[string]any)
	assert.Equal(t, "abc123", source[fHash])
}

func TestMigrateDecision_APIVersionDetection(t *testing.T) {
	migrator := New()
	input := map[string]any{
		fAPIVersion: fAPIVersionGroup + VersionV2,
		fWorkload: map[string]any{
			fKind: "Deployment", fNamespace: "default", fName: "web",
		},
		fSources:      []any{},
		fShouldReload: false,
	}
	out, plan, err := migrator.MigrateDecision(input, VersionV3)
	require.NoError(t, err)
	assert.Equal(t, VersionV2, plan.SourceVersion)
	assert.Equal(t, VersionV3, out[fSchemaVersion])
}

func TestMigrateDecision_Idempotent(t *testing.T) {
	migrator := New()
	input := v1Decision()
	// Unknown fields at root and inside nested objects must survive repeated
	// migration without being wrapped twice.
	input["experimentalTTL"] = 30
	input["nestedExtra"] = map[string]any{"zone": "east"}

	first, _, err := migrator.MigrateDecision(input, VersionV3)
	require.NoError(t, err)

	second, secondPlan, err := migrator.MigrateDecision(first, VersionV3)
	require.NoError(t, err)
	third, _, err := migrator.MigrateDecision(second, VersionV3)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, second, third)
	assert.Equal(t, first[fDecisionID], third[fDecisionID])

	markers := third[fUnknownFields].(map[string]any)
	require.Contains(t, markers, "experimentalTTL")
	ttl := markers["experimentalTTL"].(map[string]any)
	assert.Equal(t, true, ttl["preserved"])
	assert.Equal(t, VersionV1, ttl["detectedInVersion"])
	assert.NotEmpty(t, ttl["note"])
	// The marker itself must not be re-collected as an unknown field.
	assert.NotContains(t, markers, fUnknownFields)

	// The second run only performs safe no-op style steps, never re-defaults.
	assert.False(t, hasStep(secondPlan.Steps, fStrategy, ActionDefaulted))
	assert.False(t, hasStep(secondPlan.Steps, fEvent+"."+fType, ActionDefaulted))
}

func TestMigrateDecision_UnknownAndDowngradeVersions(t *testing.T) {
	migrator := New()

	out, plan, err := migrator.MigrateDecision(v1Decision(), "v42")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnknownVersion))
	assert.Nil(t, out)
	assert.Equal(t, "v42", plan.TargetVersion)
	assert.NotContains(t, err.Error(), "app-secret")

	out, _, err = migrator.MigrateDecision(
		map[string]any{fSchemaVersion: "v7", fWorkload: "x"}, VersionV3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnknownVersion))
	assert.Nil(t, out)

	out, _, err = migrator.MigrateDecision(
		map[string]any{fSchemaVersion: 9, fWorkload: "x"}, VersionV3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnknownVersion) || errors.Is(err, ErrInvalidField))
	assert.Nil(t, out)

	v3, _, err := migrator.MigrateDecision(v1Decision(), VersionV3)
	require.NoError(t, err)
	out, _, err = migrator.MigrateDecision(v3, VersionV2)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDowngrade))
	assert.Nil(t, out)
}

func TestMigrateDecision_InvalidFieldsFailClosed(t *testing.T) {
	migrator := New()

	cases := []map[string]any{
		{"namespace": "default"}, // missing workload
		{"workload": 7},          // wrong workload type
		{"workload": "x", "reload": "yes"},
		{"workload": "x", fSources: "not-an-array"},
		{"workload": "x", fSources: []any{map[string]any{fName: "x"}}},
	}
	for _, input := range cases {
		out, _, err := migrator.MigrateDecision(input, VersionV2)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidField), "expected invalid field error, got %v", err)
		assert.Nil(t, out)
	}
}

func TestMigrateDecision_FieldConflict_Bool(t *testing.T) {
	input := map[string]any{
		"workload":     "web",
		"namespace":    "default",
		"reload":       false,
		fShouldReload:  true,
		"auto":         true,
		fAutoReload:    false,
		fSchemaVersion: VersionV1,
	}

	t.Run("default fails without producing an object", func(t *testing.T) {
		migrator := New()
		out, plan, err := migrator.MigrateDecision(input, VersionV2)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFieldConflict))
		assert.Nil(t, out)
		// Fail-fast: the first conflicting pair is recorded before aborting.
		assert.True(t, hasStep(plan.Steps, fV1Reload, ActionConflict))
		// Errors name fields only, never values.
		assert.NotContains(t, err.Error(), "true")
	})

	t.Run("auto conflict is also detected on its own", func(t *testing.T) {
		migrator := New()
		autoOnly := map[string]any{
			"workload":     "web",
			"namespace":    "default",
			"auto":         true,
			fAutoReload:    false,
			fSchemaVersion: VersionV1,
		}
		out, plan, err := migrator.MigrateDecision(autoOnly, VersionV2)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrFieldConflict))
		assert.Nil(t, out)
		assert.True(t, hasStep(plan.Steps, fV1Auto, ActionConflict))
	})

	t.Run("prefer legacy", func(t *testing.T) {
		migrator := New()
		out, plan, err := migrator.MigrateDecision(input, VersionV2, WithConflictPolicy(ConflictPreferLegacy))
		require.NoError(t, err)
		assert.Equal(t, false, out[fShouldReload])
		assert.Equal(t, true, out[fAutoReload])
		assert.True(t, hasStep(plan.Steps, fV1Reload, ActionConflict))
	})

	t.Run("prefer current", func(t *testing.T) {
		migrator := New()
		out, _, err := migrator.MigrateDecision(input, VersionV2, WithConflictPolicy(ConflictPreferCurrent))
		require.NoError(t, err)
		assert.Equal(t, true, out[fShouldReload])
		assert.Equal(t, false, out[fAutoReload])
	})
}

func TestMigrateDecision_FieldConflict_Sources(t *testing.T) {
	input := map[string]any{
		"workload":     "web",
		"namespace":    "default",
		"configmap":    "legacy-cm",
		fSchemaVersion: VersionV1,
		fSources: []any{
			map[string]any{fType: constants.ConfigmapEnvVarPostfix, fName: "other-cm"},
		},
	}

	migrator := New()
	out, plan, err := migrator.MigrateDecision(input, VersionV2)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFieldConflict))
	assert.Nil(t, out)
	assert.True(t, hasStep(plan.Steps, fSources, ActionConflict))

	out, _, err = migrator.MigrateDecision(input, VersionV2, WithConflictPolicy(ConflictPreferLegacy))
	require.NoError(t, err)
	sources := out[fSources].([]any)
	require.Len(t, sources, 1)
	assert.Equal(t, "legacy-cm", sources[0].(map[string]any)[fName])

	out, _, err = migrator.MigrateDecision(input, VersionV2, WithConflictPolicy(ConflictPreferCurrent))
	require.NoError(t, err)
	sources = out[fSources].([]any)
	assert.Equal(t, "other-cm", sources[0].(map[string]any)[fName])

	// Agreement (same type/name, different ordering/extra metadata) is fine.
	agree := map[string]any{
		"workload":     "web",
		"namespace":    "default",
		"configmap":    "same-cm",
		fSchemaVersion: VersionV1,
		fSources: []any{
			map[string]any{
				fType: constants.ConfigmapEnvVarPostfix, fName: "same-cm", fHash: "deadbeef",
			},
		},
	}
	out, _, err = migrator.MigrateDecision(agree, VersionV2)
	require.NoError(t, err)
	assert.Len(t, out[fSources].([]any), 1)
}

func TestMigrateDecision_NeverMutatesInput(t *testing.T) {
	migrator := New()

	t.Run("success does not mutate input", func(t *testing.T) {
		input := v1Decision()
		expected := mustJSON(t, input)
		_, _, err := migrator.MigrateDecision(input, VersionV3)
		require.NoError(t, err)
		assert.JSONEq(t, expected, mustJSON(t, input))
	})

	t.Run("failure does not mutate input", func(t *testing.T) {
		input := map[string]any{
			"workload":     "web",
			"reload":       false,
			fShouldReload:  true,
			fSchemaVersion: VersionV1,
		}
		expected := mustJSON(t, input)
		out, _, err := migrator.MigrateDecision(input, VersionV2)
		require.Error(t, err)
		assert.Nil(t, out)
		assert.JSONEq(t, expected, mustJSON(t, input))
	})

	t.Run("json bytes are detached", func(t *testing.T) {
		out, _, err := migrator.MigrateDecision(json.RawMessage(mustJSON(t, v1Decision())), VersionV3)
		require.NoError(t, err)
		assert.Equal(t, VersionV3, out[fSchemaVersion])
	})
}

func TestMigrateDecision_UnknownFieldsSafeMarkers(t *testing.T) {
	migrator := New()
	input := map[string]any{
		"workload":      "web",
		"namespace":     "default",
		"futureFlag":    map[string]any{"enabled": true},
		"workload_kind": "Deployment", // unknown: v1 uses workloadKind
	}

	out, plan, err := migrator.MigrateDecision(input, VersionV3)
	require.NoError(t, err)

	envelope := out[fUnknownFields].(map[string]any)
	assert.Contains(t, envelope, "futureFlag")
	assert.Contains(t, envelope, "workload_kind")
	for _, raw := range envelope {
		marker := raw.(map[string]any)
		assert.Equal(t, true, marker["preserved"])
		assert.Equal(t, VersionV1, marker["detectedInVersion"])
		assert.NotEmpty(t, marker["note"])
	}
	assert.True(t, hasStep(plan.Steps, "futureFlag", ActionPreservedUnknown))
	assert.True(t, hasStep(plan.Steps, "workload_kind", ActionPreservedUnknown))

	// Preserved values stay out of typed decision fields.
	decision, err := DecodeV3(out)
	require.NoError(t, err)
	assert.Contains(t, decision.UnknownFields, "futureFlag")
	assert.True(t, decision.UnknownFields["futureFlag"].Preserved)

	// String rendering must not include unknown values.
	rendered := decision.String()
	assert.NotContains(t, rendered, "futureFlag-value")
	assert.Contains(t, rendered, "unknownFieldKeys")
}

func TestMigrateDecision_NestedUnknownFieldsAreCollected(t *testing.T) {
	migrator := New()
	input := map[string]any{
		fSchemaVersion: VersionV2,
		fWorkload: map[string]any{
			fKind: "Deployment", fNamespace: "default", fName: "web", "zone": "east",
		},
		fSources: []any{
			map[string]any{
				fType: constants.ConfigmapEnvVarPostfix, fName: "cm", "custom": "x",
			},
		},
	}
	out, plan, err := migrator.MigrateDecision(input, VersionV3)
	require.NoError(t, err)

	envelope := out[fUnknownFields].(map[string]any)
	assert.Contains(t, envelope, "workload.zone")
	assert.Contains(t, envelope, "sources[0].custom")
	assert.True(t, hasStep(plan.Steps, "workload.zone", ActionPreservedUnknown))

	// The nested object no longer carries the unknown key.
	workload := out[fWorkload].(map[string]any)
	assert.NotContains(t, workload, "zone")
}

func TestMigrateDecision_SecretValuesNeverLeak(t *testing.T) {
	const secretValue = "do-not-leak-this-token-value"

	migrator := New()
	input := map[string]any{
		"workload":     "web",
		"namespace":    "default",
		"configmap":    "cm",
		fSchemaVersion: VersionV1,
		// Accidental payloads that must never travel with a decision.
		"token": secretValue,
		"secretPayload": map[string]any{
			"data": map[string]any{"db-password": secretValue},
		},
		fSources: []any{
			map[string]any{
				fType:  constants.ConfigmapEnvVarPostfix,
				fName:  "cm", // agrees with the legacy configmap field
				"data": map[string]any{"api_key": secretValue},
			},
		},
	}

	out, plan, err := migrator.MigrateDecision(input, VersionV3)
	require.NoError(t, err)

	assert.NotContains(t, mustJSON(t, out), secretValue)
	assert.NotContains(t, plan.String(), secretValue)
	assert.True(t, hasStep(plan.Steps, "token", ActionRedacted))
	assert.True(t, hasStep(plan.Steps, "secretPayload.data", ActionRedacted))
	assert.True(t, hasStep(plan.Steps, "sources[0].data", ActionRedacted))

	// Redacted content is preserved only as a safe marker, never the value.
	envelope := out[fUnknownFields].(map[string]any)
	payload := envelope["secretPayload"].(map[string]any)
	assert.Equal(t, RedactionMarker, payload["value"].(map[string]any)["data"])
	nestedSource := envelope["sources[0].data"].(map[string]any)
	assert.Equal(t, RedactionMarker, nestedSource["value"])

	// The caller's own input is untouched and still holds its own value.
	assert.Equal(t, secretValue, input["token"])
}

func TestMigrateDecision_ErrorMessagesNeverLeakValues(t *testing.T) {
	const secretValue = "super-secret-conflict-value"
	migrator := New()
	input := map[string]any{
		"workload":     "web",
		"namespace":    "default",
		"reload":       false,
		fShouldReload:  true,
		fSchemaVersion: VersionV1,
		"credentials":  secretValue,
	}
	out, plan, err := migrator.MigrateDecision(input, VersionV2)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secretValue)
	assert.NotContains(t, plan.String(), secretValue)
	assert.Nil(t, out)
}

func TestMigrateDecision_Rollback(t *testing.T) {
	migrator := New()
	input := v1Decision()
	input["experimental"] = "kept"

	_, plan, err := migrator.MigrateDecision(input, VersionV3)
	require.NoError(t, err)

	restored, err := migrator.Rollback(plan)
	require.NoError(t, err)
	assert.Equal(t, input, restored)

	// Rollback returns a detached copy.
	restored["workload"] = "mutated"
	again, err := migrator.Rollback(plan)
	require.NoError(t, err)
	assert.Equal(t, "checkout", again["workload"])
}

func TestMigrateDecision_RollbackAfterFailureIsScrubbed(t *testing.T) {
	const secretValue = "rollback-secret-value"
	migrator := New()
	input := map[string]any{
		"workload":     "web",
		"reload":       false,
		fShouldReload:  true,
		fSchemaVersion: VersionV1,
		"stringData":   map[string]any{"password": secretValue},
	}
	out, plan, err := migrator.MigrateDecision(input, VersionV2)
	require.Error(t, err)
	assert.Nil(t, out)

	restored, rollbackErr := migrator.Rollback(plan)
	require.NoError(t, rollbackErr)
	assert.NotContains(t, mustJSON(t, restored), secretValue)
	assert.Equal(t, RedactionMarker, restored["stringData"])

	// Caller input is still intact; rollback only covers the scrubbed copy.
	assert.Equal(t, secretValue, input["stringData"].(map[string]any)["password"])
}

func TestMigrateDecision_RollbackRequiresSnapshot(t *testing.T) {
	migrator := New()
	_, err := migrator.Rollback(&Plan{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoRollback))

	_, err = migrator.Rollback(nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoRollback))
}

func TestMigrateDecision_PlanContainsFieldLevelSteps(t *testing.T) {
	migrator := New()
	_, plan, err := migrator.MigrateDecision(v1Decision(), VersionV3)
	require.NoError(t, err)

	for _, step := range plan.Steps {
		assert.NotEmpty(t, step.Path)
		assert.NotEmpty(t, step.Action)
		assert.NotEmpty(t, step.FromVersion)
		assert.NotEmpty(t, step.ToVersion)
		// Steps must never embed values: guard against accidental payloads.
		assert.False(t, strings.Contains(step.Detail, "app-secret"))
	}

	paths := map[string]bool{}
	for _, step := range plan.Steps {
		paths[step.Path] = true
	}
	assert.True(t, paths[fWorkload])
	assert.True(t, paths[fV1Configmap])
	assert.True(t, paths[fV1Secret])
	assert.True(t, paths[fStrategy])
}

func TestMigrateDecision_IdentityV1TagsVersion(t *testing.T) {
	migrator := New()
	input := v1Decision()
	out, _, err := migrator.MigrateDecision(input, VersionV1)
	require.NoError(t, err)
	assert.Equal(t, VersionV1, out[fSchemaVersion])
}
