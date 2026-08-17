package project

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			"pod name suffix",
			"Readiness probe failed for checkout-api-7d9f8b6c5d-x2k9p",
			"Readiness probe failed for checkout-api-<pod>",
		},
		{
			"ip and port",
			"Get \"http://10.4.2.17:8080/healthz\": dial tcp 10.4.2.17:8080: connect: connection refused",
			"Get \"http://<ip>/healthz\": dial tcp <ip>: connect: connection refused",
		},
		{
			"image digest",
			"Failed to pull image \"app@sha256:1f2e3d4c5b6a7988990a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f607\"",
			"Failed to pull image \"app@<digest>\"",
		},
		{
			"uid",
			"Unable to attach volume for pod 3f2504e0-4f89-11d3-9a0c-0305e82c3301",
			"Unable to attach volume for pod <uid>",
		},
		{
			"duration",
			"Liveness probe failed after 1.503s",
			"Liveness probe failed after <dur>",
		},
		{
			"collapses whitespace",
			"  Back-off   restarting failed container  ",
			"Back-off restarting failed container",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEventsCollapse is the context-budget claim made concrete: a realistic burst of per-pod
// events must reduce to a handful of lines without losing the total count.
func TestEventsCollapse(t *testing.T) {
	now := time.Now()
	var raw []corev1.Event

	// 40 pods each reporting the same two failures, with pod-specific names and IPs in the message.
	for i := 0; i < 40; i++ {
		pod := fmt.Sprintf("checkout-api-7d9f8b6c5d-%05d", i)
		raw = append(raw,
			ev("Warning", "BackOff", "Back-off restarting failed container app in pod "+pod, pod, 3, now),
			ev("Warning", "Unhealthy", fmt.Sprintf("Readiness probe failed: dial tcp 10.4.%d.%d:8080: connect: connection refused", i/250, i%250), pod, 5, now),
		)
	}

	got := Events(raw, now)
	if len(got) != 2 {
		t.Fatalf("collapsed 80 events into %d groups, want 2:\n%+v", len(got), got)
	}

	total := int32(0)
	for _, g := range got {
		total += g.Count
	}
	if want := int32(40*3 + 40*5); total != want {
		t.Errorf("total count = %d, want %d — dedup must not lose occurrences", total, want)
	}
	// Warnings sort first; both are warnings here, so just confirm the reasons survived.
	seen := map[string]bool{got[0].Reason: true, got[1].Reason: true}
	if !seen["BackOff"] || !seen["Unhealthy"] {
		t.Errorf("lost a reason: %+v", got)
	}
	// Collapsing must not hide the blast radius: the group has to say how many pods it covers.
	for _, g := range got {
		if g.ObjectCount != 40 {
			t.Errorf("%s covers %d objects, want 40 — collapsing lost the blast radius", g.Reason, g.ObjectCount)
		}
		if g.ObjectName == "" {
			t.Errorf("%s has no example object to drill into", g.Reason)
		}
	}
}

// TestEventsExampleIsDeterministic pins that the representative object does not depend on map
// iteration order — otherwise committed fixtures flap between runs for no real reason.
func TestEventsExampleIsDeterministic(t *testing.T) {
	now := time.Now()
	raw := []corev1.Event{
		ev("Warning", "BackOff", "Back-off restarting", "pod-c", 1, now),
		ev("Warning", "BackOff", "Back-off restarting", "pod-a", 1, now),
		ev("Warning", "BackOff", "Back-off restarting", "pod-b", 1, now),
	}
	for i := 0; i < 20; i++ {
		got := Events(raw, now)
		if len(got) != 1 || got[0].ObjectName != "pod-a" {
			t.Fatalf("run %d: got example %q, want the lexically first (pod-a)", i, got[0].ObjectName)
		}
	}
}

// TestEventsUseSeriesCount pins that we trust the apiserver's own aggregation. Recounting locally
// would undercount by orders of magnitude on exactly the events that matter most.
func TestEventsUseSeriesCount(t *testing.T) {
	now := time.Now()
	e := ev("Warning", "BackOff", "Back-off restarting failed container", "pod-a", 0, now)
	e.Series = &corev1.EventSeries{Count: 512, LastObservedTime: metav1.NewMicroTime(now.Add(-30 * time.Second))}

	got := Events([]corev1.Event{e}, now)
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if got[0].Count != 512 {
		t.Errorf("count = %d, want 512 from the series field", got[0].Count)
	}
	if got[0].LastSeenSecondsAgo != 30 {
		t.Errorf("last seen = %ds ago, want 30 from series.lastObservedTime", got[0].LastSeenSecondsAgo)
	}
}

// TestEventsCountDefaultsToOne covers events.k8s.io writers that leave Count unset entirely.
func TestEventsCountDefaultsToOne(t *testing.T) {
	now := time.Now()
	got := Events([]corev1.Event{ev("Normal", "Scheduled", "Successfully assigned prod/pod-a", "pod-a", 0, now)}, now)
	if len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("got %+v, want one group with count 1", got)
	}
}

// TestWarningsSortFirst pins the reading order: warnings before normal, newest before oldest.
func TestWarningsSortFirst(t *testing.T) {
	now := time.Now()
	got := Events([]corev1.Event{
		ev("Normal", "Pulled", "Container image already present", "pod-a", 1, now),
		ev("Warning", "BackOff", "Back-off restarting", "pod-a", 1, now.Add(-time.Hour)),
	}, now)
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2", len(got))
	}
	if got[0].Type != "Warning" {
		t.Errorf("first group is %q/%q; warnings must sort first even when older", got[0].Type, got[0].Reason)
	}
}

func ev(typ, reason, msg, obj string, count int32, at time.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: obj + ".x", CreationTimestamp: metav1.NewTime(at)},
		Type:           typ,
		Reason:         reason,
		Message:        msg,
		Count:          count,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: obj},
		FirstTimestamp: metav1.NewTime(at.Add(-5 * time.Minute)),
		LastTimestamp:  metav1.NewTime(at),
	}
}
