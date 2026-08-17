package detect

import (
	"strings"
	"testing"

	"github.com/backendArchitect/argus/internal/model"
)

// onNode builds a workload whose pods sit on a named node and were OOM-killed. The node carries
// MemoryPressure, so the node detector fires and suppresses the workload-level finding — the same
// scope widening diagnose_workload does, now happening once per workload in a scan.
func onNode(name, node string, pressure bool) *model.Snapshot {
	s := &model.Snapshot{
		Scope:     "workload/prod/" + name,
		Namespace: "prod",
		Workload: &model.WorkloadView{
			Kind: "Deployment", Name: name, Namespace: "prod",
			Desired: 2, Ready: 0, Selector: map[string]string{"app": name},
		},
		Pods: []model.PodView{{
			Name: name + "-abc-1", Node: node, Phase: "Running", OwnerName: name + "-abc",
			Containers: []model.ContainerView{{
				ContainerSpecView: model.ContainerSpecView{Name: "app", LimitMem: "128Mi"},
				State:             model.ContainerStateView{Status: "waiting", Reason: "CrashLoopBackOff"},
				LastState: &model.ContainerStateView{
					Status: "terminated", Reason: "OOMKilled", ExitCode: 137, SecondsAgo: 30,
				},
				RestartCount: 4,
			}},
		}},
	}
	if pressure {
		s.Nodes = []model.NodeView{{
			Name: node, Ready: true,
			Conditions: []model.ConditionView{{
				Type: "MemoryPressure", Status: "True", Reason: "KubeletHasInsufficientMemory",
				Message: "kubelet has insufficient memory available", LastChangeSecsAgo: 300,
			}},
		}}
	}
	return s
}

// TestTriageCollapsesInfrastructureFindings is the reason triage is not just a loop over diagnose.
//
// Three workloads on one unhealthy node would each report an identical node.unhealthy-host finding.
// That is the per-pod noise problem repeated one level up, and it is exactly what triage exists to
// prevent — so the node is reported once, with a count of how many workloads it affects.
func TestTriageCollapsesInfrastructureFindings(t *testing.T) {
	snaps := []*model.Snapshot{
		onNode("checkout", "node-a", true),
		onNode("orders", "node-a", true),
		onNode("search", "node-a", true),
	}
	r := Triage("cluster", snaps, nil, nil)

	if len(r.Cluster) != 1 {
		t.Fatalf("got %d infrastructure findings, want 1 collapsed: %+v", len(r.Cluster), r.Cluster)
	}
	if r.Cluster[0].ID != "node.unhealthy-host" {
		t.Errorf("cluster finding = %q", r.Cluster[0].ID)
	}
	// The count is the difference between "a node is unwell" and "a node is taking out three services".
	var found bool
	for _, e := range r.Cluster[0].Evidence {
		if strings.Contains(e.Excerpt, "3 workload(s)") {
			found = true
		}
	}
	if !found {
		t.Errorf("the collapsed finding does not say how many workloads it affects: %+v", r.Cluster[0].Evidence)
	}
	// And the node explains the OOMKills, so no workload should be reported separately.
	if len(r.Groups) != 0 {
		t.Errorf("got %d workload groups, want 0 — the node finding subsumes them: %+v", len(r.Groups), r.Groups)
	}
}

// TestTriageKeepsWorkloadFindingsWhenTheNodeIsFine is the other half. Without a node problem, each
// workload owns its own diagnosis and must be reported individually.
func TestTriageKeepsWorkloadFindingsWhenTheNodeIsFine(t *testing.T) {
	snaps := []*model.Snapshot{
		onNode("checkout", "node-a", false),
		onNode("orders", "node-b", false),
	}
	r := Triage("cluster", snaps, nil, nil)

	if len(r.Cluster) != 0 {
		t.Errorf("got %d infrastructure findings on healthy nodes: %+v", len(r.Cluster), r.Cluster)
	}
	if len(r.Groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(r.Groups))
	}
	if r.Scanned != 2 || r.Unhealthy != 2 {
		t.Errorf("scanned=%d unhealthy=%d, want 2 and 2", r.Scanned, r.Unhealthy)
	}
	for _, g := range r.Groups {
		if g.Kind != "Deployment" || g.Namespace != "prod" {
			t.Errorf("group lost its identity: %+v", g)
		}
		if len(g.Findings) == 0 {
			t.Errorf("group %s has no findings but was reported", g.Name)
		}
	}
}

// TestTriageIgnoresHealthyWorkloads guards the signal-to-noise property. A scan that lists all 165
// workloads is a workload inventory, not a triage.
func TestTriageIgnoresHealthyWorkloads(t *testing.T) {
	healthy := &model.Snapshot{
		Scope:     "workload/prod/fine",
		Namespace: "prod",
		Workload: &model.WorkloadView{
			Kind: "Deployment", Name: "fine", Namespace: "prod", Desired: 2, Ready: 2,
		},
		Pods: []model.PodView{{
			Name: "fine-1", Phase: "Running", Ready: true,
			Containers: []model.ContainerView{{
				ContainerSpecView: model.ContainerSpecView{Name: "app"},
				Ready:             true,
				State:             model.ContainerStateView{Status: "running", SecondsAgo: 9000},
			}},
		}},
	}
	r := Triage("cluster", []*model.Snapshot{healthy, onNode("broken", "node-a", false)}, nil, nil)

	if r.Scanned != 2 {
		t.Errorf("scanned = %d, want 2", r.Scanned)
	}
	if r.Unhealthy != 1 {
		t.Errorf("unhealthy = %d, want 1 — the healthy workload must not be listed", r.Unhealthy)
	}
	for _, g := range r.Groups {
		if g.Name == "fine" {
			t.Error("a healthy workload appeared in the triage output")
		}
	}
}

// TestTriageCapsOutput checks the report stays readable and reports what it dropped. Silent
// truncation reads as "that is everything" when it is not.
func TestTriageCapsOutput(t *testing.T) {
	var snaps []*model.Snapshot
	for i := 0; i < MaxTriageGroups+7; i++ {
		snaps = append(snaps, onNode(string(rune('a'+i))+"-svc", "node-"+string(rune('a'+i)), false))
	}
	r := Triage("cluster", snaps, nil, nil)

	if len(r.Groups) != MaxTriageGroups {
		t.Errorf("got %d groups, want the cap of %d", len(r.Groups), MaxTriageGroups)
	}
	if r.Omitted != 7 {
		t.Errorf("omitted = %d, want 7", r.Omitted)
	}
	if !strings.Contains(RenderTriage(r), "7 further workload(s)") {
		t.Error("the rendered report does not say how many were omitted")
	}
}

// TestRenderTriageSaysNothingIsNotProof pins the wording when a scan comes up empty. Zero findings
// across a cluster is easy to read as "everything is fine", which is a stronger claim than argus can
// make: it means none of these detectors matched.
func TestRenderTriageSaysNothingIsNotProof(t *testing.T) {
	out := RenderTriage(Triage("cluster", nil, nil, nil))
	if !strings.Contains(out, "not the same as") {
		t.Errorf("an empty triage must not imply the cluster is healthy:\n%s", out)
	}
	for _, id := range IDs() {
		if !strings.Contains(out, id) {
			t.Errorf("empty triage should list what was checked; missing %s", id)
		}
	}
}
