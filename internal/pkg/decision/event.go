package decision

import (
	"bytes"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stakater/Reloader/pkg/common"
)

// ResourceRef is a non-sensitive workload identifier used in phased errors.
// It never carries data, annotations or labels of the triggering ConfigMap or
// Secret.
type ResourceRef struct {
	Kind      string
	Namespace string
	Name      string
}

// String renders Kind/namespace/name or Kind/name for cluster-scoped objects.
func (r ResourceRef) String() string {
	if r.Namespace == "" {
		return fmt.Sprintf("%s/%s", r.Kind, r.Name)
	}
	return fmt.Sprintf("%s/%s/%s", r.Kind, r.Namespace, r.Name)
}

func (r ResourceRef) key() string {
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

// Event is a unit of work: one triggering resource change evaluated against one
// caller-provided workload object.
//
// Config carries only metadata and the SHA of the changed resource; it never
// contains raw Secret values. Workload is read but never mutated and never
// refetched: the engine has no Kubernetes client.
type Event struct {
	// Config describes the changed ConfigMap, Secret or SecretProviderClass.
	Config common.Config
	// Kind is the workload kind, e.g. "Deployment".
	Kind string
	// Workload is the object supplied by the caller.
	Workload runtime.Object
}

// eventRef extracts a safe reference and validates the event without rendering
// any caller supplied values in error messages.
func eventRef(ev Event) (ResourceRef, *PhaseError) {
	if ev.Workload == nil || ev.Kind == "" || ev.Config.ResourceName == "" || ev.Config.Type == "" {
		return ResourceRef{}, newPhaseError(PhaseEvaluate, ReasonInvalidEvent, ResourceRef{Kind: ev.Kind}, ErrInvalidEvent, 0, 0)
	}
	accessor, err := meta.Accessor(ev.Workload)
	if err != nil {
		return ResourceRef{Kind: ev.Kind}, newPhaseError(PhaseEvaluate, ReasonInvalidEvent, ResourceRef{Kind: ev.Kind}, ErrInvalidEvent, 0, 0)
	}
	return ResourceRef{
		Kind:      ev.Kind,
		Namespace: accessor.GetNamespace(),
		Name:      accessor.GetName(),
	}, nil
}

// estimateObjectSize writes the JSON encoding of obj into scratch and returns
// its size. The buffer is a temporary resource owned by the caller of this
// function: it is reset by the engine after each event, including abort paths.
// Secret data is never present because trigger Config only carries hashes; the
// encoded workload is never rendered anywhere.
func estimateObjectSize(scratch *bytes.Buffer, obj runtime.Object) (int64, error) {
	scratch.Reset()
	if err := json.NewEncoder(scratch).Encode(obj); err != nil {
		return 0, err
	}
	return int64(scratch.Len()), nil
}
