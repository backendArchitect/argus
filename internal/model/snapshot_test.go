package model

import (
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// full is a Snapshot with every field set, so the round-trip test fails if a new field is added
// without a working tag rather than silently dropping it.
func full() *Snapshot {
	return &Snapshot{
		Scope:     "workload/prod/checkout-api",
		Namespace: "prod",
		Workload: &WorkloadView{
			Kind: "Deployment", Name: "checkout-api", Namespace: "prod",
			Labels: map[string]string{"app": "checkout-api"}, Selector: map[string]string{"app": "checkout-api"},
			Desired: 3, Ready: 0, Updated: 3, Available: 0,
			Generation: 12, ObservedGeneration: 12, CreatedSecondsAgo: 86400,
			Conditions: []ConditionView{{Type: "Available", Status: "False", Reason: "MinimumReplicasUnavailable"}},
		},
		ReplicaSets: []ReplicaSetView{{
			Name: "checkout-api-7d9f", Revision: "12", Desired: 3, Current: true,
			Template: []ContainerSpecView{{Name: "app", Image: "app:v2", LimitMem: "128Mi"}},
		}},
		Pods: []PodView{{
			Name: "checkout-api-7d9f-x2k9p", Node: "gke-abc", Phase: "Running",
			Labels: map[string]string{"app": "checkout-api"}, OwnerKind: "ReplicaSet",
			OwnerName: "checkout-api-7d9f", CreatedSecondsAgo: 300,
			Containers: []ContainerView{{
				ContainerSpecView: ContainerSpecView{
					Name: "app", Image: "app:v2", LimitMem: "128Mi", RequestCPU: "100m",
					EnvKeys:   []string{"DATABASE_URL"},
					Readiness: &ProbeView{Kind: "http", InitialDelay: 1, Period: 10, FailureThreshold: 3},
				},
				RestartCount: 7,
				State:        ContainerStateView{Status: "waiting", Reason: "CrashLoopBackOff"},
				LastState:    &ContainerStateView{Status: "terminated", Reason: "OOMKilled", ExitCode: 137, SecondsAgo: 42},
				UsageMem:     "119Mi",
			}},
		}},
		Events:   []EventGroup{{Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Count: 104}},
		Services: []ServiceView{{Name: "checkout-api", Selector: map[string]string{"app": "checkout-api"}, MatchedPods: 3}},
		Nodes:    []NodeView{{Name: "gke-abc", Ready: true, Taints: []string{"k=v:NoSchedule"}}},
		HPA:      &HPAView{Name: "checkout-api", MinReplicas: 2, MaxReplicas: 10, Current: 3},
		PDB:      &PDBView{Name: "checkout-api", DesiredHealthy: 2, CurrentHealthy: 0},
		Degraded: []string{"metrics: the server could not find the requested resource"},
	}
}

// TestSnapshotRoundTrip is the load-bearing constraint: `argus capture` writes YAML, the detector
// tests read it back, and production runs the same struct. If a field does not survive the trip,
// fixtures silently lose it and a detector that depends on it stops firing in tests only.
func TestSnapshotRoundTrip(t *testing.T) {
	want := full()

	data, err := yaml.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Snapshot
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(want, &got) {
		t.Errorf("snapshot did not survive the round trip\n--- yaml ---\n%s", data)
	}
}

// TestEmbeddedSpecIsInlined pins the ContainerView embedding. encoding/json promotes an anonymous
// struct's fields only when the tag has no name; a stray name there would nest the spec under a
// key and every detector reading c.Image would read empty.
func TestEmbeddedSpecIsInlined(t *testing.T) {
	data, err := yaml.Marshal(full().Pods[0].Containers[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ContainerSpecView") {
		t.Errorf("container spec is nested, not inlined:\n%s", data)
	}
	if !strings.Contains(string(data), "image: app:v2") {
		t.Errorf("image not promoted to the top level:\n%s", data)
	}
}

func TestParseQuantities(t *testing.T) {
	for _, tc := range []struct {
		in       string
		cpu, mem int64
	}{
		{"", 0, 0},
		{"garbage", 0, 0}, // must degrade to unknown, never panic
		{"100m", 100, 0},
		{"1", 1000, 1},
		{"128Mi", 0, 134217728},
	} {
		if got := ParseCPU(tc.in); tc.cpu != 0 && got != tc.cpu {
			t.Errorf("ParseCPU(%q) = %d, want %d", tc.in, got, tc.cpu)
		}
		if got := ParseMem(tc.in); tc.mem != 0 && got != tc.mem {
			t.Errorf("ParseMem(%q) = %d, want %d", tc.in, got, tc.mem)
		}
	}
}

// TestProbeDeadline pins the arithmetic the readiness detector depends on.
func TestProbeDeadline(t *testing.T) {
	var nilProbe *ProbeView
	if got := nilProbe.Deadline(); got != 0 {
		t.Errorf("nil probe deadline = %d, want 0", got)
	}
	p := &ProbeView{InitialDelay: 5, Period: 10, FailureThreshold: 3}
	if got := p.Deadline(); got != 35 {
		t.Errorf("deadline = %d, want 35 (5 + 10*3)", got)
	}
}
