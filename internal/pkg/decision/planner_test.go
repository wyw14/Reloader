package decision

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stakater/Reloader/internal/pkg/constants"
	"github.com/stakater/Reloader/internal/pkg/options"
	"github.com/stakater/Reloader/pkg/common"
)

func testOptions() *common.ReloaderOptions {
	return common.GetCommandLineOptions()
}

func secretConfig(sha string) common.Config {
	return common.Config{
		Namespace:           "test-ns",
		ResourceName:        "db-secret",
		ResourceAnnotations: map[string]string{},
		Annotation:          options.SecretUpdateOnChangeAnnotation,
		TypedAutoAnnotation: options.SecretReloaderAutoAnnotation,
		SHAValue:            sha,
		Type:                constants.SecretEnvVarPostfix,
	}
}

func namedSecretDeployment(name, secretName string, envs []v1.EnvVar) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-ns",
			Annotations: map[string]string{
				options.SecretUpdateOnChangeAnnotation: secretName,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: "app", Env: envs}},
				},
			},
		},
	}
}

func plannerEvent(obj runtime.Object, sha string) Event {
	kind := "Deployment"
	switch obj.(type) {
	case *batchv1.CronJob:
		kind = "CronJob"
	case *batchv1.Job:
		kind = "Job"
	case *appsv1.StatefulSet:
		kind = "StatefulSet"
	case *appsv1.DaemonSet:
		kind = "DaemonSet"
	}
	return Event{Config: secretConfig(sha), Kind: kind, Workload: obj}
}

func TestDefaultPlannerAddsEnvVar(t *testing.T) {
	dep := namedSecretDeployment("web", "db-secret", nil)

	plan, needsReload, err := DefaultPlanner(context.Background(), plannerEvent(dep, "sha-1"), testOptions())
	require.NoError(t, err)
	assert.True(t, needsReload)
	assert.Equal(t, constants.Updated, plan.Result)
	require.NotNil(t, plan.Patch)
	assert.Equal(t, "STAKATER_DB_SECRET_SECRET", plan.EnvVar)
	assert.Contains(t, string(plan.Patch.Bytes), "STAKATER_DB_SECRET_SECRET")
	assert.Contains(t, string(plan.Patch.Bytes), "sha-1")
}

func TestDefaultPlannerDetectsUpToDateEnvVar(t *testing.T) {
	envs := []v1.EnvVar{{Name: "STAKATER_DB_SECRET_SECRET", Value: "sha-1"}}
	dep := namedSecretDeployment("web", "db-secret", envs)

	plan, needsReload, err := DefaultPlanner(context.Background(), plannerEvent(dep, "sha-1"), testOptions())
	require.NoError(t, err)
	assert.True(t, needsReload)
	assert.Equal(t, constants.NotUpdated, plan.Result)
	assert.Nil(t, plan.Patch)
}

func TestDefaultPlannerDetectsStaleEnvVar(t *testing.T) {
	envs := []v1.EnvVar{{Name: "STAKATER_DB_SECRET_SECRET", Value: "old-sha"}}
	dep := namedSecretDeployment("web", "db-secret", envs)

	plan, needsReload, err := DefaultPlanner(context.Background(), plannerEvent(dep, "new-sha"), testOptions())
	require.NoError(t, err)
	assert.True(t, needsReload)
	assert.Equal(t, constants.Updated, plan.Result)
	assert.Contains(t, string(plan.Patch.Bytes), "new-sha")
}

func TestDefaultPlannerSkipsUnrelatedWorkload(t *testing.T) {
	dep := namedSecretDeployment("web", "other-secret", nil)

	plan, needsReload, err := DefaultPlanner(context.Background(), plannerEvent(dep, "sha-1"), testOptions())
	require.NoError(t, err)
	assert.False(t, needsReload)
	assert.Equal(t, constants.Result(0), plan.Result)
}

func TestDefaultPlannerNoContainer(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty",
			Namespace: "test-ns",
			Annotations: map[string]string{
				options.SecretUpdateOnChangeAnnotation: "db-secret",
			},
		},
	}

	plan, needsReload, err := DefaultPlanner(context.Background(), plannerEvent(dep, "sha-1"), testOptions())
	require.NoError(t, err)
	assert.True(t, needsReload)
	assert.Equal(t, constants.NoContainerFound, plan.Result)
}

func TestDefaultPlannerAnnotationsStrategy(t *testing.T) {
	opts := testOptions()
	opts.ReloadStrategy = constants.AnnotationsReloadStrategy
	dep := namedSecretDeployment("web", "db-secret", nil)

	plan, needsReload, err := DefaultPlanner(context.Background(), plannerEvent(dep, "sha-1"), opts)
	require.NoError(t, err)
	assert.True(t, needsReload)
	assert.Equal(t, constants.Updated, plan.Result)
	require.NotNil(t, plan.Patch)
	patchText := string(plan.Patch.Bytes)
	assert.Contains(t, patchText, constants.ReloaderAnnotationPrefix+"/"+constants.LastReloadedFromAnnotation)
	assert.Contains(t, patchText, "db-secret")
	assert.Contains(t, patchText, "sha-1")
}

func TestDefaultPlannerSecretKeyRefTargeting(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "test-ns",
			Annotations: map[string]string{
				options.ReloaderAutoAnnotation: "true",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name: "app",
						Env: []v1.EnvVar{{
							Name: "PASSWORD",
							ValueFrom: &v1.EnvVarSource{
								SecretKeyRef: &v1.SecretKeySelector{
									LocalObjectReference: v1.LocalObjectReference{Name: "db-secret"},
								},
							},
						}},
					}},
				},
			},
		},
	}

	plan, needsReload, err := DefaultPlanner(context.Background(), plannerEvent(dep, "sha-1"), testOptions())
	require.NoError(t, err)
	assert.True(t, needsReload)
	assert.True(t, plan.AutoReload)
	assert.Equal(t, constants.Updated, plan.Result)
}

func TestDefaultPlannerCronJobNeedsFullUpdate(t *testing.T) {
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "batch",
			Namespace: "test-ns",
			Annotations: map[string]string{
				options.SecretUpdateOnChangeAnnotation: "db-secret",
			},
		},
		Spec: batchv1.CronJobSpec{
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{{Name: "worker"}},
						},
					},
				},
			},
		},
	}

	plan, needsReload, err := DefaultPlanner(context.Background(), plannerEvent(cronJob, "sha-1"), testOptions())
	require.NoError(t, err)
	assert.True(t, needsReload)
	assert.Equal(t, constants.Updated, plan.Result)
	assert.Nil(t, plan.Patch, "CronJobs are updated, not patched, by the live handlers")
}

func TestDefaultPlannerRejectsUnsupportedWorkload(t *testing.T) {
	ev := Event{
		Config: secretConfig("sha-1"),
		Kind:   "Pod",
		Workload: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "test-ns"},
		},
	}

	_, _, err := DefaultPlanner(context.Background(), ev, testOptions())
	assert.ErrorIs(t, err, ErrUnsupportedWorkload)
}

func TestDefaultPlannerDoesNotMutateWorkload(t *testing.T) {
	dep := namedSecretDeployment("web", "db-secret", nil)
	envsBefore := len(dep.Spec.Template.Spec.Containers[0].Env)

	_, _, err := DefaultPlanner(context.Background(), plannerEvent(dep, "sha-1"), testOptions())
	require.NoError(t, err)
	assert.Len(t, dep.Spec.Template.Spec.Containers[0].Env, envsBefore, "planner must not append env vars to the passed object")
}

func TestDefaultPlannerObservesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := DefaultPlanner(ctx, plannerEvent(namedSecretDeployment("web", "db-secret", nil), "sha-1"), testOptions())
	assert.ErrorIs(t, err, context.Canceled)
	assert.True(t, strings.Contains(err.Error(), "context canceled"))
}
