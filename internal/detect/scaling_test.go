package detect

import (
	"strings"
	"testing"

	"github.com/backendArchitect/argus/internal/model"
)

// TestPDBHealthGate is the test that decides whether this detector is safe to ship.
//
// disruptionsAllowed reaching 0 is not itself interesting: it is what the budget is FOR, and every
// genuinely broken workload has it. Firing on that would put a confident, irrelevant warning on
// top of every real outage — the exact false-positive behaviour that makes a diagnostic tool get
// muted. The finding only means something when the workload has met its bar and the budget still
// leaves no room, which makes the constraint structural.
func TestPDBHealthGate(t *testing.T) {
	tests := []struct {
		name                                    string
		desiredHealthy, currentHealthy, allowed int32
		wantFinding                             bool
		why                                     string
	}{
		{
			name: "healthy but pinned", desiredHealthy: 1, currentHealthy: 1, allowed: 0,
			wantFinding: true,
			why:         "the real finding: 1 replica, minAvailable 1, blocks every drain forever",
		},
		{
			name: "healthy with headroom", desiredHealthy: 1, currentHealthy: 3, allowed: 2,
			wantFinding: false,
			why:         "a correctly sized budget is not a finding",
		},
		{
			name: "workload is broken", desiredHealthy: 3, currentHealthy: 1, allowed: 0,
			wantFinding: false,
			why:         "THE important negative: the budget is a symptom of the outage, not a cause",
		},
		{
			name: "scaled to zero", desiredHealthy: 0, currentHealthy: 0, allowed: 0,
			wantFinding: false,
			why:         "nothing to disrupt and no drain to block; matches the scaled-to-zero rule",
		},
		{
			name: "exactly at the bar", desiredHealthy: 2, currentHealthy: 2, allowed: 0,
			wantFinding: true,
			why:         "minAvailable equal to the replica count is the other way to get here",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := scalingSnapshot()
			s.PDB = &model.PDBView{
				Name: "checkout-api", DesiredHealthy: tc.desiredHealthy,
				CurrentHealthy: tc.currentHealthy, DisruptionsAllowed: tc.allowed,
			}
			got := detectPDBBlocksDrain(s)
			if (len(got) > 0) != tc.wantFinding {
				t.Errorf("got %d findings, want finding=%v\n  why: %s", len(got), tc.wantFinding, tc.why)
			}
			if len(got) > 0 && got[0].Severity != model.Warning {
				t.Errorf("severity = %q, want warning: the workload is serving, so this must "+
					"never outrank the critical that explains an outage", got[0].Severity)
			}
		})
	}

	// No PDB at all is the common case and must be silent.
	if got := detectPDBBlocksDrain(scalingSnapshot()); len(got) != 0 {
		t.Errorf("no PDB in the snapshot produced %d findings, want 0", len(got))
	}
}

// TestHPACannotScale pins the condition gating. The detector reads the controller's own verdict
// rather than inferring one from replica counts, because a workload sitting steady at its minimum
// is indistinguishable from a broken autoscaler if you only look at the numbers.
func TestHPACannotScale(t *testing.T) {
	tests := []struct {
		name        string
		conds       []model.ConditionView
		wantFinding bool
		why         string
	}{
		{
			name: "cannot get metrics",
			conds: []model.ConditionView{
				{Type: "AbleToScale", Status: "True", Reason: "SucceededGetScale"},
				{Type: "ScalingActive", Status: "False", Reason: "FailedGetResourceMetric",
					Message: "unable to get metrics for resource cpu"},
			},
			wantFinding: true, why: "the verified live case: no metrics API, so no replica count",
		},
		{
			name: "scaling normally",
			conds: []model.ConditionView{
				{Type: "AbleToScale", Status: "True", Reason: "ReadyForNewScale"},
				{Type: "ScalingActive", Status: "True", Reason: "ValidMetricFound"},
			},
			wantFinding: false, why: "a working autoscaler is not a finding",
		},
		{
			name:        "no conditions yet",
			conds:       nil,
			wantFinding: false,
			why:         "a freshly created HPA has not reported yet — must not read absence as failure",
		},
		{
			name: "only AbleToScale is false",
			conds: []model.ConditionView{
				{Type: "AbleToScale", Status: "False", Reason: "FailedGetScale"},
			},
			wantFinding: false,
			why:         "keyed on ScalingActive; AbleToScale alone is a different fault, not this one",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := scalingSnapshot()
			s.HPA = &model.HPAView{
				Name: "checkout-api", MinReplicas: 1, MaxReplicas: 5, Current: 1, Desired: 0,
				Conditions: tc.conds,
			}
			got := detectHPACannotScale(s)
			if (len(got) > 0) != tc.wantFinding {
				t.Errorf("got %d findings, want finding=%v\n  why: %s", len(got), tc.wantFinding, tc.why)
			}
			if len(got) > 0 && got[0].Severity != model.Warning {
				t.Errorf("severity = %q, want warning", got[0].Severity)
			}
		})
	}

	if got := detectHPACannotScale(scalingSnapshot()); len(got) != 0 {
		t.Errorf("no HPA in the snapshot produced %d findings, want 0", len(got))
	}
}

// TestHPANamesTheNarrowerFault checks that a controller which cannot reach the scale subresource
// at all says so, since that points at the scaleTargetRef or RBAC rather than at metrics and would
// otherwise send the reader to install metrics-server for no reason.
func TestHPANamesTheNarrowerFault(t *testing.T) {
	s := scalingSnapshot()
	s.HPA = &model.HPAView{
		Name: "checkout-api", MinReplicas: 1, MaxReplicas: 5, Current: 1,
		Conditions: []model.ConditionView{
			{Type: "AbleToScale", Status: "False", Reason: "FailedGetScale"},
			{Type: "ScalingActive", Status: "False", Reason: "FailedGetResourceMetric"},
		},
	}
	got := detectHPACannotScale(s)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if !strings.Contains(got[0].Detail, "AbleToScale is also False") {
		t.Errorf("detail does not distinguish the scale-subresource fault:\n%s", got[0].Detail)
	}
}

// scalingSnapshot is a healthy 3/3 Deployment with neither an HPA nor a PDB, so each test adds
// only the object it is about.
func scalingSnapshot() *model.Snapshot {
	return &model.Snapshot{
		Scope: "workload/prod/checkout-api", Namespace: "prod",
		Workload: &model.WorkloadView{
			Kind: "Deployment", Name: "checkout-api", Namespace: "prod",
			Desired: 3, Ready: 3, Updated: 3, Available: 3,
			Generation: 1, ObservedGeneration: 1, CreatedSecondsAgo: 8000,
		},
	}
}
