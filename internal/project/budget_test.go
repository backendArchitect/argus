package project

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podTokenBudget is the ceiling for one projected pod. Raw Kubernetes pods run to several thousand
// tokens once managedFields and last-applied-configuration are counted; a diagnosis that spends
// that per pod fills the model's context with noise before it reaches a conclusion.
//
// Measured as bytes/4, which is the standard rough conversion and errs on the pessimistic side for
// the JSON we emit.
const podTokenBudget = 400

func tokens(t *testing.T, v any) int {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return len(b) / 4
}

// realisticPod mirrors the shape that actually blew the budget in testing: a workload with 93
// environment variables, a sidecar, and a long registry path. Names are generic — the test is
// about token counts and env-value redaction, not about any particular deployment.
func realisticPod() *corev1.Pod {
	env := make([]corev1.EnvVar, 93)
	for i := range env {
		env[i] = corev1.EnvVar{Name: "SOME_REASONABLY_LONG_CONFIG_KEY_" + strings.Repeat("X", i%12), Value: "secret-value"}
	}
	mounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/etc/config"},
		{Name: "kube-api-access-abcde", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount"},
	}
	main := corev1.Container{
		Name:         "app",
		Image:        "registry.example.com/example-project/orders-api:a1b2c3d",
		Env:          env,
		EnvFrom:      []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}}}},
		VolumeMounts: mounts,
		Args:         []string{"--config", "/etc/config/app.yaml", "--verbose"},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"}},
			InitialDelaySeconds: 5, PeriodSeconds: 10, FailureThreshold: 3, TimeoutSeconds: 1,
		},
	}
	side := corev1.Container{Name: "swagger", Image: "registry.example.com/example-project/sidecar:v1", Env: env}

	started := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "orders-api-5fdc4d8b5-n264k",
			Namespace:         "shop",
			Labels:            map[string]string{"app": "orders-api", "pod-template-hash": "5fdc4d8b5"},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: corev1.PodSpec{NodeName: "node-pool-a-da01ef02-253e", Containers: []corev1.Container{main, side}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true, Started: &started, RestartCount: 7,
					State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}},
				{Name: "swagger", Ready: true, Started: &started},
			},
		},
	}
}

// TestPodBudget is the context-discipline gate. It caught a real 2.4x overrun: env keys were being
// repeated on every pod despite being a property of the shared pod template.
func TestPodBudget(t *testing.T) {
	got := tokens(t, Pod(realisticPod(), nil, time.Now()))
	if got > podTokenBudget {
		t.Errorf("projected pod is ~%d tokens, budget is %d", got, podTokenBudget)
	}
	t.Logf("projected pod ~%d tokens (budget %d)", got, podTokenBudget)
}

// TestPodProjectionDropsTemplateBulk pins *why* the pod fits: template-level fields live on the
// ReplicaSet, not repeated per pod. Without this the budget test could be satisfied by trimming
// something a detector actually needs.
func TestPodProjectionDropsTemplateBulk(t *testing.T) {
	v := Pod(realisticPod(), nil, time.Now())
	c := v.Containers[0]
	if len(c.EnvKeys) != 0 || len(c.EnvFrom) != 0 || len(c.Args) != 0 || len(c.Mounts) != 0 {
		t.Errorf("pod containers carry template-level bulk: env=%d envFrom=%d args=%d mounts=%d",
			len(c.EnvKeys), len(c.EnvFrom), len(c.Args), len(c.Mounts))
	}
	// ...but the fields detectors need must survive.
	if c.LimitMem == "" || c.RequestCPU == "" || c.Readiness == nil || c.Image == "" {
		t.Errorf("projection dropped a field a detector needs: %+v", c)
	}
	if c.LastState == nil || c.LastState.Reason != "OOMKilled" {
		t.Errorf("lastState lost; the OOM detector reads it: %+v", c.LastState)
	}
}

// TestTemplateKeepsEnvKeys is the other half: the rollout diff needs the full env key list, so the
// ReplicaSet template must still carry what the pod projection drops.
func TestTemplateKeepsEnvKeys(t *testing.T) {
	spec := ContainerSpec(&realisticPod().Spec.Containers[0])
	if len(spec.EnvKeys) != 93 {
		t.Errorf("template has %d env keys, want 93 — the rollout diff cannot see changes it drops", len(spec.EnvKeys))
	}
	if len(spec.EnvFrom) != 1 || spec.EnvFrom[0] != "secret/app-config" {
		t.Errorf("envFrom = %v, want [secret/app-config]", spec.EnvFrom)
	}
}

// TestNoEnvValuesEverEmitted is a redaction gate, not a budget one: env values are the single most
// likely place for a credential to reach model context, and a key name is all any detector needs.
func TestNoEnvValuesEverEmitted(t *testing.T) {
	b, err := json.Marshal(Pod(realisticPod(), nil, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret-value") {
		t.Fatal("an environment variable VALUE reached the output")
	}
	if b2, _ := json.Marshal(ContainerSpec(&realisticPod().Spec.Containers[0])); strings.Contains(string(b2), "secret-value") {
		t.Fatal("an environment variable VALUE reached the template projection")
	}
}

// TestServiceAccountMountElided keeps the auto-injected token mount out: it is on every pod in
// every cluster and tells a detector nothing.
func TestServiceAccountMountElided(t *testing.T) {
	spec := ContainerSpec(&realisticPod().Spec.Containers[0])
	for _, m := range spec.Mounts {
		if strings.Contains(m, "serviceaccount") {
			t.Errorf("service account token mount emitted: %q", m)
		}
	}
	if len(spec.Mounts) != 1 {
		t.Errorf("mounts = %v, want just the config mount", spec.Mounts)
	}
}
