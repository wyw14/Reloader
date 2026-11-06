package migration

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func legacyEvent() map[string]any {
	return map[string]any{
		fType: EventUpdate,
		fEventObject: map[string]any{
			fAPIVersion: "v1",
			fKind:       "Secret",
			fEventMetadata: map[string]any{
				fName:            "app-secret",
				fNamespace:       "shop",
				fResourceVersion: "42",
			},
		},
	}
}

func TestMigrateEvent_V1ToV3(t *testing.T) {
	migrator := New()
	out, plan, err := migrator.MigrateEvent(legacyEvent(), VersionV3)
	require.NoError(t, err)

	assert.Equal(t, VersionV1, plan.SourceVersion)
	assert.Equal(t, VersionV3, plan.TargetVersion)
	assert.Equal(t, VersionV3, out[fSchemaVersion])
	assert.Equal(t, EventUpdate, out[fType])

	resource := out[fResource].(map[string]any)
	assert.Equal(t, "v1", resource[fAPIVersion])
	assert.Equal(t, "Secret", resource[fKind])
	assert.Equal(t, "app-secret", resource[fName])
	assert.Equal(t, "shop", resource[fNamespace])
	assert.Equal(t, "42", resource[fResourceVersion])

	assert.True(t, hasStep(plan.Steps, fEventObject+"."+fKind, ActionRenamed))
	assert.True(t, hasStep(plan.Steps,
		fEventObject+"."+fEventMetadata+"."+fResourceVersion, ActionRenamed))
}

func TestMigrateEvent_DefaultsTypeAndPreservesUnknownMetadata(t *testing.T) {
	migrator := New()
	event := legacyEvent()
	delete(event, fType) // untagged events predate explicit event types
	object := event[fEventObject].(map[string]any)
	metadata := object[fEventMetadata].(map[string]any)
	metadata[fUID] = "abc-123" // not part of v3 resource reference

	out, plan, err := migrator.MigrateEvent(event, VersionV3)
	require.NoError(t, err)

	assert.Equal(t, EventUpdate, out[fType])
	assert.True(t, hasStep(plan.Steps, fType, ActionDefaulted))

	envelope := out[fUnknownFields].(map[string]any)
	expectedPath := fEventObject + "." + fEventMetadata + "." + fUID
	assert.Contains(t, envelope, expectedPath)
	marker := envelope[expectedPath].(map[string]any)
	assert.Equal(t, true, marker["preserved"])
	assert.Equal(t, "abc-123", marker["value"])
}

func TestMigrateEvent_SecretPayloadIsRedacted(t *testing.T) {
	const secretValue = "event-secret-payload-value"
	migrator := New()

	event := legacyEvent()
	object := event[fEventObject].(map[string]any)
	object["data"] = map[string]any{"password": secretValue}
	object["stringData"] = map[string]any{"token": secretValue}

	out, plan, err := migrator.MigrateEvent(event, VersionV3)
	require.NoError(t, err)

	assert.NotContains(t, mustJSON(t, out), secretValue)
	assert.NotContains(t, plan.String(), secretValue)
	assert.True(t, hasStep(plan.Steps, fEventObject+".data", ActionRedacted))
	assert.True(t, hasStep(plan.Steps, fEventObject+".stringData", ActionRedacted))

	envelope := out[fUnknownFields].(map[string]any)
	data := envelope[fEventObject+".data"].(map[string]any)
	assert.Equal(t, RedactionMarker, data["value"])

	// Input is untouched.
	assert.Equal(t, secretValue, object["data"].(map[string]any)["password"])
}

func TestMigrateEvent_Idempotent(t *testing.T) {
	migrator := New()
	first, _, err := migrator.MigrateEvent(legacyEvent(), VersionV3)
	require.NoError(t, err)

	second, plan, err := migrator.MigrateEvent(first, VersionV3)
	require.NoError(t, err)
	third, _, err := migrator.MigrateEvent(second, VersionV3)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, second, third)
	for _, step := range plan.Steps {
		assert.NotEqual(t, ActionDefaulted, step.Action)
	}
}

func TestMigrateEvent_UnknownAndInvalid(t *testing.T) {
	migrator := New()

	out, _, err := migrator.MigrateEvent(legacyEvent(), "v9")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnknownVersion))
	assert.Nil(t, out)

	tagged := legacyEvent()
	tagged[fSchemaVersion] = "v7"
	out, _, err = migrator.MigrateEvent(tagged, VersionV3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnknownVersion))
	assert.Nil(t, out)

	missingObject := map[string]any{fType: EventUpdate}
	out, plan, err := migrator.MigrateEvent(missingObject, VersionV3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidField))
	assert.Nil(t, out)
	assert.True(t, hasStep(plan.Steps, fType, ActionCopied))

	missingName := legacyEvent()
	metadata := missingName[fEventObject].(map[string]any)[fEventMetadata].(map[string]any)
	delete(metadata, fName)
	out, _, err = migrator.MigrateEvent(missingName, VersionV3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidField))
	assert.Nil(t, out)

	badKind := legacyEvent()
	badKind[fEventObject].(map[string]any)[fKind] = 5
	out, _, err = migrator.MigrateEvent(badKind, VersionV3)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidField))
	assert.Nil(t, out)
}

func TestMigrateEvent_Rollback(t *testing.T) {
	const secretValue = "event-rollback-secret"
	migrator := New()

	event := legacyEvent()
	event[fEventObject].(map[string]any)["data"] = map[string]any{"key": secretValue}

	out, plan, err := migrator.MigrateEvent(event, VersionV3)
	require.NoError(t, err)
	require.NotNil(t, out)

	restored, err := migrator.Rollback(plan)
	require.NoError(t, err)
	assert.Equal(t, EventUpdate, restored[fType])
	assert.NotContains(t, mustJSON(t, restored), secretValue)
	assert.Equal(t, RedactionMarker, restored[fEventObject].(map[string]any)["data"])

	// Original input is never written back.
	assert.Equal(t, secretValue,
		event[fEventObject].(map[string]any)["data"].(map[string]any)["key"])
}

func TestMigrateEvent_JSONInput(t *testing.T) {
	migrator := New()
	raw, err := json.Marshal(legacyEvent())
	require.NoError(t, err)

	out, _, err := migrator.MigrateEvent(json.RawMessage(raw), VersionV3)
	require.NoError(t, err)
	assert.Equal(t, VersionV3, out[fSchemaVersion])
}
