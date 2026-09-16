package detect

import (
	"strings"
	"testing"

	"github.com/backendArchitect/argus/internal/model"
)

// TestStartupProbeGates pins every condition that has to hold before this detector speaks.
//
// The negative cases carry the weight. This detector claims a CRITICAL at high confidence off a
// handful of fields, and the thing that earns that is the narrowness of the gate — a startup probe
// configured, never passed, restarts climbing, and a termination by signal. Loosen any one of them
// and it starts explaining ordinary application crashes as probe misconfiguration, which sends the
// reader to edit a manifest while their service stays down.
func TestStartupProbeGates(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*model.ContainerView)
		wantFinding bool
		why         string
	}{
		{
			name:        "the verified live case",
			mutate:      func(c *model.ContainerView) {},
			wantFinding: true,
			why:         "90s boot, 6s budget, killed by SIGKILL with started=false",
		},
		{
			name:        "no startup probe configured",
			mutate:      func(c *model.ContainerView) { c.Startup = nil },
			wantFinding: false,
			why:         "with no startup probe there is no budget to have exceeded",
		},
		{
			name:        "the startup probe passed",
			mutate:      func(c *model.ContainerView) { c.Started = true },
			wantFinding: false,
			why:         "started=true means it got through; whatever is wrong now is something else",
		},
		{
			name:        "only one restart",
			mutate:      func(c *model.ContainerView) { c.RestartCount = 1 },
			wantFinding: false,
			why:         "a single restart is not a loop — the same rule detectCrashLoop applies",
		},
		{
			name: "the application exited on its own",
			mutate: func(c *model.ContainerView) {
				c.LastState.ExitCode = 1
				c.LastState.Reason = "Error"
			},
			wantFinding: false,
			why: "THE important negative: exit 1 is the app's own code, so it crashed rather " +
				"than being killed. detectCrashLoop owns that and must not be overridden",
		},
		{
			name: "killed by the OOM killer",
			mutate: func(c *model.ContainerView) {
				c.LastState.Reason = "OOMKilled" // also exit 137
			},
			wantFinding: false,
			why:         "shares exit 137 but is a different fix entirely; detectOOMKill owns it",
		},
		{
			name: "no previous termination recorded",
			mutate: func(c *model.ContainerView) {
				c.LastState = nil
			},
			wantFinding: false,
			why:         "without a termination there is no evidence anything killed it",
		},
		{
			name: "SIGTERM rather than SIGKILL",
			mutate: func(c *model.ContainerView) {
				c.LastState.ExitCode = 143
			},
			wantFinding: true,
			why:         "the kubelet asks politely before it insists; both are external kills",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := startupSnapshot()
			tc.mutate(&s.Pods[0].Containers[0])
			got := detectStartupProbeTooShort(s)
			if (len(got) > 0) != tc.wantFinding {
				t.Errorf("got %d findings, want finding=%v\n  why: %s", len(got), tc.wantFinding, tc.why)
			}
			if len(got) > 0 && got[0].Severity != model.Critical {
				t.Errorf("severity = %q, want critical: the workload is 0/1 and cannot recover "+
					"on its own", got[0].Severity)
			}
		})
	}
}

// TestStartupSuppressesTheCrashLoopSymptom is the reason this detector is worth having rather than
// merely accurate.
//
// On the timing where the container IS caught terminated, detectCrashLoop fires too — exit 137 is
// not in byExitCode, so it lands on the crashloop.exiting-nonzero fallback, whose advice is to go
// read the logs. That is actively misleading here: the application is booting correctly and the
// logs show nothing wrong. One cause, reported once.
func TestStartupSuppressesTheCrashLoopSymptom(t *testing.T) {
	s := startupSnapshot()
	// Catch it terminated rather than mid-boot, which is what lets the crash-loop detector in.
	s.Pods[0].Containers[0].State = model.ContainerStateView{
		Status: "waiting", Reason: "CrashLoopBackOff",
		Message: "back-off 40s restarting failed container",
	}

	// Establish the premise: without suppression the crash-loop detector really does claim this.
	if got := detectCrashLoop(s); len(got) == 0 {
		t.Fatal("premise broken: detectCrashLoop no longer fires here, so this test proves nothing")
	} else if got[0].ID != "crashloop.exiting-nonzero" {
		t.Logf("note: crash-loop now reports %q rather than the exiting-nonzero fallback", got[0].ID)
	}

	ids := map[string]bool{}
	for _, f := range All(s) {
		ids[f.ID] = true
	}
	if !ids["probe.startup-too-short"] {
		t.Error("the startup finding did not survive ranking")
	}
	for _, sup := range []string{"crashloop.exiting-nonzero", "crashloop.terminated-on-signal"} {
		if ids[sup] {
			t.Errorf("%s was reported alongside the startup finding — one cause stated twice", sup)
		}
	}
}

// TestStartupPointsAtThePreviousInstance checks the next step, which differs from the config
// detectors on purpose: here logs DO exist and are the way to find the real boot time, so the hint
// has to ask for the killed instance rather than the current one.
func TestStartupPointsAtThePreviousInstance(t *testing.T) {
	got := detectStartupProbeTooShort(startupSnapshot())
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	h := got[0].NextTool
	if h == nil || h.Tool != "get_workload_logs" {
		t.Fatalf("NextTool = %+v, want get_workload_logs", h)
	}
	if h.Args["previous"] != "true" {
		t.Errorf("hint asks for the current instance; the killed one is what shows how far it got")
	}
	// The budget has to reach the reader, since raising it is the fix.
	if !strings.Contains(got[0].Detail, "6s") {
		t.Errorf("detail does not state the computed budget:\n%s", got[0].Detail)
	}
}

// TestStartupProbeEventIsOptional: the kubelet's event carries the probe's actual complaint, which
// separates "too slow" from "pointed at the wrong thing". Events expire after about an hour, so the
// finding must stand without one — at lower confidence, since that is the corroboration lost.
func TestStartupProbeEventIsOptional(t *testing.T) {
	with := detectStartupProbeTooShort(startupSnapshot())

	s := startupSnapshot()
	s.Events = nil
	without := detectStartupProbeTooShort(s)

	if len(without) != 1 {
		t.Fatalf("got %d findings with no events, want 1 — the structural signal stands alone",
			len(without))
	}
	if !(without[0].Confidence < with[0].Confidence) {
		t.Errorf("confidence without the event (%.2f) is not lower than with it (%.2f)",
			without[0].Confidence, with[0].Confidence)
	}
}

// startupSnapshot is the shape captured from a live cluster: a 90s boot on a 6s budget, caught
// RUNNING partway through a window it never finishes, which is the timing that made this invisible
// to every other detector.
func startupSnapshot() *model.Snapshot {
	return &model.Snapshot{
		Scope: "workload/prod/slow-boot", Namespace: "prod",
		Workload: &model.WorkloadView{
			Kind: "Deployment", Name: "slow-boot", Namespace: "prod",
			Desired: 1, Ready: 0, Updated: 1, Available: 0,
			Generation: 1, ObservedGeneration: 1, CreatedSecondsAgo: 400,
		},
		ReplicaSets: []model.ReplicaSetView{{
			Name: "slow-boot-86f496d648", Revision: "1", Current: true,
			Desired: 1, Ready: 0, Available: 0, CreatedSecondsAgo: 400,
		}},
		Pods: []model.PodView{{
			Name: "slow-boot-86f496d648-s69sj", Phase: "Running", Ready: false,
			Node: "node-1", OwnerKind: "ReplicaSet", OwnerName: "slow-boot-86f496d648",
			CreatedSecondsAgo: 400,
			Containers: []model.ContainerView{{
				ContainerSpecView: model.ContainerSpecView{
					Name: "app", Image: "busybox:1.36",
					Startup: &model.ProbeView{
						Kind: "exec", InitialDelay: 0, Period: 2, Timeout: 1, FailureThreshold: 3,
					},
				},
				Ready: false, Started: false, RestartCount: 5,
				State: model.ContainerStateView{Status: "running", SecondsAgo: 4},
				LastState: &model.ContainerStateView{
					Status: "terminated", Reason: "Error", ExitCode: 137, SecondsAgo: 10,
				},
			}},
		}},
		Events: []model.EventGroup{{
			Type: "Warning", Reason: "Unhealthy",
			Message:     "Startup probe failed: cat: can't open '/tmp/ready': No such file or directory",
			Count:       10,
			ObjectKind:  "Pod",
			ObjectName:  "slow-boot-86f496d648-s69sj",
			ObjectCount: 1,
		}},
	}
}
