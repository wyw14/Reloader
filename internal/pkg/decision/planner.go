package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	patchtypes "k8s.io/apimachinery/pkg/types"

	"github.com/stakater/Reloader/internal/pkg/constants"
	"github.com/stakater/Reloader/internal/pkg/util"
	"github.com/stakater/Reloader/pkg/common"
)

// Patch describes the strategic merge patch a caller may apply to perform the
// reload. Bytes only contain the SHA of the trigger resource, never raw Secret
// values.
type Patch struct {
	Type  patchtypes.PatchType
	Bytes []byte
}

// Plan is the evaluated, not yet committed reload decision for one workload.
type Plan struct {
	// AutoReload mirrors ReloadCheckResult.AutoReload.
	AutoReload bool
	// Result follows the constants.Result semantics used by the reload handlers.
	Result constants.Result
	// EnvVar is the STAKATER_* environment variable used by the env-vars strategy.
	EnvVar string
	// Patch is the strategic merge patch for patchable workloads; nil for
	// workloads that require a full update (Job/CronJob).
	Patch *Patch
	// Notes carries non-fatal problems, e.g. invalid reload annotations.
	Notes []error
}

// Planner evaluates a single caller-provided workload against the triggering
// event.
//
// Implementations MUST be pure: they may only read the passed Event and
// options, must not contact the Kubernetes API or any network service, and must
// not mutate the passed workload. The context exists solely so long-running
// planning can observe cancellation.
//
// needsReload is false when the workload does not reference the changed
// resource; plan still describes the outcome when it is true.
type Planner func(ctx context.Context, ev Event, opts *common.ReloaderOptions) (plan Plan, needsReload bool, err error)

// DefaultPlanner is the built-in, cluster-free planner. It supports the
// pod-template workloads (Deployment, StatefulSet, DaemonSet) as well as Job and
// CronJob. Reload matching reuses pkg/common.ShouldReload so decision semantics
// stay identical to the live reload path.
func DefaultPlanner(ctx context.Context, ev Event, opts *common.ReloaderOptions) (Plan, bool, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, false, err
	}

	view, ok := extractWorkloadView(ev)
	if !ok {
		return Plan{}, false, ErrUnsupportedWorkload
	}

	check := common.ShouldReload(ev.Config, ev.Kind, common.Map(view.annotations), common.Map(view.podAnnotations), opts)
	if !check.ShouldReload {
		return Plan{Notes: check.Errors}, false, nil
	}

	container := targetContainer(view, ev.Config, check.AutoReload)
	if container == nil {
		return Plan{AutoReload: check.AutoReload, Result: constants.NoContainerFound, Notes: check.Errors}, true, nil
	}

	if opts != nil && opts.ReloadStrategy == constants.AnnotationsReloadStrategy {
		return planAnnotations(view, container, ev.Config, check.AutoReload, check.Errors), true, nil
	}
	return planEnvVars(view, container, ev.Config, check.AutoReload, check.Errors), true, nil
}

// workloadView is the read-only projection of a workload used by the planner.
type workloadView struct {
	kind           string
	annotations    map[string]string
	podAnnotations map[string]string
	containers     []v1.Container
	initContainers []v1.Container
	volumes        []v1.Volume
	supportsPatch  bool
}

// extractWorkloadView projects the supported workload kinds. It never mutates
// the passed object and returns ok=false for unsupported kinds.
func extractWorkloadView(ev Event) (workloadView, bool) {
	view := workloadView{kind: ev.Kind}
	switch obj := ev.Workload.(type) {
	case *appsv1.Deployment:
		view.annotations = obj.Annotations
		view.podAnnotations = obj.Spec.Template.Annotations
		view.containers = obj.Spec.Template.Spec.Containers
		view.initContainers = obj.Spec.Template.Spec.InitContainers
		view.volumes = obj.Spec.Template.Spec.Volumes
		view.supportsPatch = true
	case *appsv1.StatefulSet:
		view.annotations = obj.Annotations
		view.podAnnotations = obj.Spec.Template.Annotations
		view.containers = obj.Spec.Template.Spec.Containers
		view.initContainers = obj.Spec.Template.Spec.InitContainers
		view.volumes = obj.Spec.Template.Spec.Volumes
		view.supportsPatch = true
	case *appsv1.DaemonSet:
		view.annotations = obj.Annotations
		view.podAnnotations = obj.Spec.Template.Annotations
		view.containers = obj.Spec.Template.Spec.Containers
		view.initContainers = obj.Spec.Template.Spec.InitContainers
		view.volumes = obj.Spec.Template.Spec.Volumes
		view.supportsPatch = true
	case *batchv1.Job:
		view.annotations = obj.Annotations
		view.podAnnotations = obj.Spec.Template.Annotations
		view.containers = obj.Spec.Template.Spec.Containers
		view.initContainers = obj.Spec.Template.Spec.InitContainers
		view.volumes = obj.Spec.Template.Spec.Volumes
	case *batchv1.CronJob:
		view.annotations = obj.Annotations
		view.podAnnotations = obj.Spec.JobTemplate.Spec.Template.Annotations
		view.containers = obj.Spec.JobTemplate.Spec.Template.Spec.Containers
		view.initContainers = obj.Spec.JobTemplate.Spec.Template.Spec.InitContainers
		view.volumes = obj.Spec.JobTemplate.Spec.Template.Spec.Volumes
	default:
		return workloadView{}, false
	}
	return view, true
}

// planEnvVars mirrors the handler env-vars strategy without touching the
// cluster or mutating the workload.
func planEnvVars(view workloadView, container *v1.Container, config common.Config, auto bool, notes []error) Plan {
	envVar := envVarName(config.ResourceName, config.Type)
	plan := Plan{AutoReload: auto, EnvVar: envVar, Notes: notes, Result: constants.Updated}

	if config.Type == constants.SecretProviderClassEnvVarPostfix && envAlreadyReloaded(view.containers, envVar, config.SHAValue) {
		plan.Result = constants.NotUpdated
		return plan
	}

	switch existingEnvValue(container.Env, envVar, config.SHAValue) {
	case envMissing:
		// A new STAKATER_* env var would be appended; this triggers a reload.
		plan.Result = constants.Updated
	case envUpToDate:
		plan.Result = constants.NotUpdated
	case envStale:
		plan.Result = constants.Updated
	}

	if view.supportsPatch && plan.Result == constants.Updated {
		plan.Patch = &Patch{
			Type:  patchtypes.StrategicMergePatchType,
			Bytes: fmt.Appendf(nil, envVarPatchTemplate, container.Name, envVar, config.SHAValue),
		}
	}
	return plan
}

// planAnnotations mirrors the handler annotations strategy.
func planAnnotations(view workloadView, container *v1.Container, config common.Config, auto bool, notes []error) Plan {
	plan := Plan{AutoReload: auto, Notes: notes, Result: constants.Updated}

	if config.Type == constants.SecretProviderClassEnvVarPostfix && annotationAlreadyReloaded(view.podAnnotations, config) {
		plan.Result = constants.NotUpdated
		return plan
	}

	source := common.NewReloadSourceFromConfig(config, []string{container.Name})
	encoded, err := json.Marshal(source)
	if err != nil {
		plan.Result = constants.NotUpdated
		return plan
	}

	if view.supportsPatch {
		escaped, err := jsonStringEscape(string(encoded))
		if err != nil {
			plan.Result = constants.NotUpdated
			return plan
		}
		plan.Patch = &Patch{
			Type:  patchtypes.StrategicMergePatchType,
			Bytes: fmt.Appendf(nil, annotationPatchTemplate, lastReloadedAnnotationKey(), escaped),
		}
	}
	return plan
}

// targetContainer selects the container that references the trigger resource,
// mirroring internal/pkg/handler.getContainerUsingResource.
func targetContainer(view workloadView, config common.Config, autoReload bool) *v1.Container {
	volumeName := volumeMountName(view.volumes, config.Type, config.ResourceName)
	if volumeName != "" {
		if container := containerWithVolumeMount(view.containers, volumeName); container != nil {
			return container
		}
		if container := containerWithVolumeMount(view.initContainers, volumeName); container != nil {
			// Init-container reference: the saved env var goes to the first pod container.
			return firstContainer(view.containers)
		}
	}

	if container := containerWithEnvReference(view.containers, config.ResourceName, config.Type); container != nil {
		return container
	}
	if container := containerWithEnvReference(view.initContainers, config.ResourceName, config.Type); container != nil {
		return firstContainer(view.containers)
	}

	// Explicit (named) annotation without a detectable reference targets the first container.
	if !autoReload {
		return firstContainer(view.containers)
	}
	return nil
}

func firstContainer(containers []v1.Container) *v1.Container {
	if len(containers) == 0 {
		return nil
	}
	return &containers[0]
}

func volumeMountName(volumes []v1.Volume, resourceType, resourceName string) string {
	for i := range volumes {
		switch resourceType {
		case constants.ConfigmapEnvVarPostfix:
			if volumes[i].ConfigMap != nil && volumes[i].ConfigMap.Name == resourceName {
				return volumes[i].Name
			}
			if volumes[i].Projected != nil {
				for j := range volumes[i].Projected.Sources {
					if volumes[i].Projected.Sources[j].ConfigMap != nil && volumes[i].Projected.Sources[j].ConfigMap.Name == resourceName {
						return volumes[i].Name
					}
				}
			}
		case constants.SecretEnvVarPostfix:
			if volumes[i].Secret != nil && volumes[i].Secret.SecretName == resourceName {
				return volumes[i].Name
			}
			if volumes[i].Projected != nil {
				for j := range volumes[i].Projected.Sources {
					if volumes[i].Projected.Sources[j].Secret != nil && volumes[i].Projected.Sources[j].Secret.Name == resourceName {
						return volumes[i].Name
					}
				}
			}
		case constants.SecretProviderClassEnvVarPostfix:
			if volumes[i].CSI != nil && volumes[i].CSI.VolumeAttributes["secretProviderClass"] == resourceName {
				return volumes[i].Name
			}
		}
	}
	return ""
}

func containerWithVolumeMount(containers []v1.Container, volumeName string) *v1.Container {
	for i := range containers {
		for j := range containers[i].VolumeMounts {
			if containers[i].VolumeMounts[j].Name == volumeName {
				return &containers[i]
			}
		}
	}
	return nil
}

func containerWithEnvReference(containers []v1.Container, resourceName, resourceType string) *v1.Container {
	for i := range containers {
		for j := range containers[i].Env {
			source := containers[i].Env[j].ValueFrom
			if source == nil {
				continue
			}
			if resourceType == constants.SecretEnvVarPostfix && source.SecretKeyRef != nil && source.SecretKeyRef.Name == resourceName {
				return &containers[i]
			}
			if resourceType == constants.ConfigmapEnvVarPostfix && source.ConfigMapKeyRef != nil && source.ConfigMapKeyRef.Name == resourceName {
				return &containers[i]
			}
		}
		for j := range containers[i].EnvFrom {
			if resourceType == constants.SecretEnvVarPostfix && containers[i].EnvFrom[j].SecretRef != nil && containers[i].EnvFrom[j].SecretRef.Name == resourceName {
				return &containers[i]
			}
			if resourceType == constants.ConfigmapEnvVarPostfix && containers[i].EnvFrom[j].ConfigMapRef != nil && containers[i].EnvFrom[j].ConfigMapRef.Name == resourceName {
				return &containers[i]
			}
		}
	}
	return nil
}

type envMatchState int

const (
	envMissing envMatchState = iota
	envUpToDate
	envStale
)

func existingEnvValue(envs []v1.EnvVar, name, sha string) envMatchState {
	for i := range envs {
		if envs[i].Name != name {
			continue
		}
		if envs[i].Value != sha {
			return envStale
		}
		return envUpToDate
	}
	return envMissing
}

// envAlreadyReloaded reports whether any container already carries the current
// SHA for the trigger; the SHA is compared against the value provided in Config.
func envAlreadyReloaded(containers []v1.Container, envVar, sha string) bool {
	for _, container := range containers {
		for _, env := range container.Env {
			if env.Name == envVar {
				return env.Value == sha
			}
		}
	}
	return false
}

func envVarName(resourceName, resourceType string) string {
	return constants.EnvVarPrefix + util.ConvertToEnvVarName(resourceName) + "_" + resourceType
}

func annotationAlreadyReloaded(podAnnotations map[string]string, config common.Config) bool {
	annotation := podAnnotations[lastReloadedAnnotationKey()]
	return strings.Contains(annotation, config.ResourceName) && strings.Contains(annotation, config.SHAValue)
}

func lastReloadedAnnotationKey() string {
	return fmt.Sprintf("%s/%s", constants.ReloaderAnnotationPrefix, constants.LastReloadedFromAnnotation)
}

func jsonStringEscape(value string) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	escaped := string(data)
	return escaped[1 : len(escaped)-1], nil
}

// Patch templates are identical to internal/pkg/callbacks.GetPatchTemplates.
const (
	envVarPatchTemplate     = `{"spec":{"template":{"spec":{"containers":[{"name":"%s","env":[{"name":"%s","value":"%s"}]}]}}}}`
	annotationPatchTemplate = `{"spec":{"template":{"metadata":{"annotations":{"%s":"%s"}}}}}`
)
