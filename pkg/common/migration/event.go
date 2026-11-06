package migration

// Legacy watch-event schema (untagged, therefore v1):
//
//	{"type":"UPDATE","object":{"apiVersion":"v1","kind":"ConfigMap",
//	  "metadata":{"name":"app","namespace":"default","resourceVersion":"42"}}}
//
// Current (v3) schema:
//
//	{"schemaVersion":"v3","type":"UPDATE",
//	  "resource":{"apiVersion":"v1","kind":"ConfigMap",
//	    "namespace":"default","name":"app","resourceVersion":"42"}}
var watchObjectSchema = schemaSpec{fields: map[string]fieldSpec{
	fAPIVersion:    {kind: scalarField},
	fKind:          {kind: scalarField},
	fEventMetadata: {kind: objectField, nested: "watchMetadata"},
}}

// watchMetadata only treats the fields carried into v3 as known; anything else
// (for example uid) is collected into the safe unknown-field envelope instead
// of being silently dropped.
var watchMetadataSchema = schemaSpec{fields: map[string]fieldSpec{
	fName:            {kind: scalarField},
	fNamespace:       {kind: scalarField},
	fResourceVersion: {kind: scalarField},
}}

var eventSchemaV1 = schemaSpec{fields: map[string]fieldSpec{
	fSchemaVersion: {kind: scalarField},
	fAPIVersion:    {kind: scalarField},
	fType:          {kind: scalarField},
	fEventObject:   {kind: objectField, nested: "watchObject"},
	fUnknownFields: {kind: scalarField},
}}

var eventSchemaV3 = schemaSpec{fields: map[string]fieldSpec{
	fSchemaVersion: {kind: scalarField},
	fAPIVersion:    {kind: scalarField},
	fType:          {kind: scalarField},
	fResource:      {kind: objectField, nested: schemaResourceRef},
	fUnknownFields: {kind: scalarField},
}}

var eventSchemas = map[string]schemaSpec{
	VersionV1:         eventSchemaV1,
	VersionV3:         eventSchemaV3,
	"watchObject":     watchObjectSchema,
	"watchMetadata":   watchMetadataSchema,
	schemaResourceRef: resourceRefSchema,
}

// eventFamily migrates watch events directly from the untagged legacy shape to
// v3; events were introduced together with the v3 decision envelope.
var eventFamily = family{
	name:    "event",
	ordered: []string{VersionV1, VersionV3},
	schemas: eventSchemas,
	hops: map[string]hopFunc{
		VersionV1 + "->" + VersionV3: hopEventV1ToV3,
	},
	finalize: func(doc map[string]any, version string) {
		doc[fSchemaVersion] = version
	},
}

// hopEventV1ToV3 flattens object/metadata into a single resource reference.
func hopEventV1ToV3(in map[string]any, _ ConflictPolicy, steps *[]FieldStep) (map[string]any, error) {
	out := map[string]any{fSchemaVersion: VersionV3}

	eventType, present, err := stringField(in, fType)
	if err != nil {
		return nil, err
	}
	if !present {
		eventType = DefaultEventType
		addStep(VersionV1, VersionV3, fType, ActionDefaulted,
			"untagged events default to UPDATE (create/delete watching defaults off)", steps)
	} else {
		addStep(VersionV1, VersionV3, fType, ActionCopied, "", steps)
	}
	out[fType] = eventType

	rawObject, ok := in[fEventObject].(map[string]any)
	if !ok {
		return nil, newInvalidFieldError(fEventObject, "must be an object")
	}

	resource, err := buildEventResource(rawObject, steps)
	if err != nil {
		return nil, err
	}
	out[fResource] = resource

	carryUnknownFields(in, out)
	return out, nil
}

// buildEventResource converts object + object.metadata into a ResourceRef.
func buildEventResource(rawObject map[string]any, steps *[]FieldStep) (map[string]any, error) {
	kind, err := requiredString(rawObject, fKind)
	if err != nil {
		return nil, err
	}
	addStep(VersionV1, VersionV3, fEventObject+"."+fKind, ActionRenamed,
		"moved into resource reference", steps)

	resource := map[string]any{fKind: kind}

	if apiVersion, _, err := stringField(rawObject, fAPIVersion); err != nil {
		return nil, err
	} else if apiVersion != "" {
		resource[fAPIVersion] = apiVersion
		addStep(VersionV1, VersionV3, fEventObject+"."+fAPIVersion, ActionCopied, "", steps)
	}

	metadata, ok := rawObject[fEventMetadata].(map[string]any)
	if !ok {
		return nil, newInvalidFieldError(fEventMetadata, "must be an object")
	}
	if err := copyMetadataScalar(metadata, resource, fName, steps); err != nil {
		return nil, err
	}
	if err := copyMetadataScalar(metadata, resource, fNamespace, steps); err != nil {
		return nil, err
	}
	if err := copyMetadataScalar(metadata, resource, fResourceVersion, steps); err != nil {
		return nil, err
	}

	return resource, nil
}

// copyMetadataScalar copies one metadata field into the resource reference.
func copyMetadataScalar(metadata, resource map[string]any, key string, steps *[]FieldStep) error {
	value, present, err := stringField(metadata, key)
	if err != nil {
		return err
	}
	if !present {
		if key == fName {
			return newInvalidFieldError(fEventMetadata+"."+fName, "is required")
		}
		return nil
	}
	resource[key] = value
	addStep(VersionV1, VersionV3, fEventObject+"."+fEventMetadata+"."+key, ActionRenamed,
		"moved into resource reference", steps)
	return nil
}

func newInvalidFieldError(path, reason string) error {
	return &fieldError{path: path, reason: reason}
}

type fieldError struct {
	path   string
	reason string
}

func (e *fieldError) Error() string {
	return ErrInvalidField.Error() + ": " + e.path + " " + e.reason
}

func (e *fieldError) Unwrap() error {
	return ErrInvalidField
}
