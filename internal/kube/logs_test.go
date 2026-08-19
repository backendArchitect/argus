package kube

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func container(name string, ready bool, restarts int32, waitingReason, lastReason string) corev1.ContainerStatus {
	cs := corev1.ContainerStatus{Name: name, Ready: ready, RestartCount: restarts}
	if waitingReason != "" {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingReason}}
	} else {
		cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	}
	if lastReason != "" {
		cs.LastTerminationState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: lastReason, ExitCode: 1},
		}
	}
	return cs
}

func pod(specNames []string, statuses ...corev1.ContainerStatus) *corev1.Pod {
	p := &corev1.Pod{}
	for _, n := range specNames {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: n})
	}
	p.Status.ContainerStatuses = statuses
	return p
}

// TestPickContainer pins the behaviour that saves the most time during an incident, and
// which the e2e gate cannot assert without racing the pod: on a crashlooping container
// the PREVIOUS instance is the one worth reading, because the current one is in backoff
// and has written nothing.
func TestPickContainer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pod          *corev1.Pod
		wantName     string
		wantPrevious bool
		reasonHas    string
	}{
		{
			name:         "crashlooping container reads the previous instance",
			pod:          pod([]string{"app"}, container("app", false, 4, "CrashLoopBackOff", "Error")),
			wantName:     "app",
			wantPrevious: true,
			reasonHas:    "PREVIOUS",
		},
		{
			name: "a container that is running reads the current instance",
			// The same workload a moment later, mid-restart. Reading current is correct here:
			// it is actually producing output.
			pod:          pod([]string{"app"}, container("app", false, 4, "", "Error")),
			wantName:     "app",
			wantPrevious: false,
		},
		{
			name: "the failing container wins over a sidecar",
			pod: pod([]string{"istio-proxy", "app"},
				container("istio-proxy", true, 0, "", ""),
				container("app", false, 3, "CrashLoopBackOff", "OOMKilled")),
			wantName:     "app",
			wantPrevious: true,
		},
		{
			name: "with nothing failing, a sidecar is still skipped",
			pod: pod([]string{"istio-proxy", "app"},
				container("istio-proxy", true, 0, "", ""),
				container("app", true, 0, "", "")),
			wantName:  "app",
			reasonHas: "skipping sidecars",
		},
		{
			name:      "a mesh component that is only sidecars still gets read",
			pod:       pod([]string{"istio-proxy"}, container("istio-proxy", true, 0, "", "")),
			wantName:  "istio-proxy",
			reasonHas: "only container",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, previous, reason := pickContainer(tc.pod, LogOptions{})
			if name != tc.wantName {
				t.Errorf("container = %q, want %q (%s)", name, tc.wantName, reason)
			}
			if previous != tc.wantPrevious {
				t.Errorf("previous = %v, want %v (%s)", previous, tc.wantPrevious, reason)
			}
			if tc.reasonHas != "" && !strings.Contains(reason, tc.reasonHas) {
				t.Errorf("reason %q should mention %q", reason, tc.reasonHas)
			}
			if reason == "" {
				t.Error("the choice must always be explained; an unexplained selection is magic")
			}
		})
	}
}

// TestPickContainerRespectsExplicitFlags checks the user can always override the
// judgement — the auto-selection is a default, not a policy.
func TestPickContainerRespectsExplicitFlags(t *testing.T) {
	p := pod([]string{"istio-proxy", "app"},
		container("istio-proxy", true, 0, "", ""),
		container("app", false, 3, "CrashLoopBackOff", "Error"))

	name, _, reason := pickContainer(p, LogOptions{Container: "istio-proxy"})
	if name != "istio-proxy" {
		t.Errorf("explicit container ignored: got %q", name)
	}
	if !strings.Contains(reason, "explicitly") {
		t.Errorf("reason should say it was requested: %q", reason)
	}

	no := false
	_, previous, reason := pickContainer(p, LogOptions{Previous: &no})
	if previous {
		t.Error("explicit -previous=false was overridden by the crashloop default")
	}
	if !strings.Contains(reason, "explicitly") {
		t.Errorf("reason should say previous was requested: %q", reason)
	}
}
