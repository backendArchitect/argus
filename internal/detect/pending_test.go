package detect

import (
	"strings"
	"testing"

	"github.com/backendArchitect/argus/internal/model"
)

const gi = 1024 * 1024 * 1024

func node(name string, cpu, mem int64, opts ...func(*model.NodeCapacity)) model.NodeCapacity {
	n := model.NodeCapacity{Name: name, Ready: true, AllocCPUMilli: cpu, AllocMemBytes: mem}
	for _, o := range opts {
		o(&n)
	}
	return n
}

func used(cpu, mem int64) func(*model.NodeCapacity) {
	return func(n *model.NodeCapacity) { n.UsedCPUMilli, n.UsedMemBytes = cpu, mem }
}
func taint(k, v, e string) func(*model.NodeCapacity) {
	return func(n *model.NodeCapacity) { n.Taints = append(n.Taints, model.Taint{Key: k, Value: v, Effect: e}) }
}
func labelled(k, v string) func(*model.NodeCapacity) {
	return func(n *model.NodeCapacity) {
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[k] = v
	}
}

// TestFitReportsTheNumbers is the whole point of the tool. "Insufficient memory" is a
// restatement of the scheduler's own message; the figures are what a human needs to
// decide whether to shrink the request or grow the cluster.
func TestFitReportsTheNumbers(t *testing.T) {
	spec := model.PendingSpec{NeedCPUMilli: 500, NeedMemBytes: 8 * gi}
	fits := Fit(spec, []model.NodeCapacity{node("n1", 4000, 16*gi, used(3800, 12*gi))})

	if fits[0].Fits {
		t.Fatal("node should not fit: 4Gi free, 8Gi requested")
	}
	joined := strings.Join(fits[0].Reasons, " | ")
	for _, want := range []string{"insufficient cpu", "needs 500m", "200m free of 4", "3800m already requested"} {
		if !strings.Contains(joined, want) {
			t.Errorf("cpu reason missing %q:\n  %s", want, joined)
		}
	}
	for _, want := range []string{"insufficient memory", "needs 8.0Gi", "4.0Gi free of 16.0Gi"} {
		if !strings.Contains(joined, want) {
			t.Errorf("memory reason missing %q:\n  %s", want, joined)
		}
	}
}

func TestFitAcceptsANodeWithRoom(t *testing.T) {
	spec := model.PendingSpec{NeedCPUMilli: 500, NeedMemBytes: 2 * gi}
	fits := Fit(spec, []model.NodeCapacity{node("roomy", 4000, 16*gi, used(1000, 4*gi))})
	if !fits[0].Fits {
		t.Errorf("node with 3 cpu and 12Gi free should fit: %v", fits[0].Reasons)
	}
}

// TestFitTaints covers the matching rules that decide whether a taint blocks. Getting
// PreferNoSchedule wrong would report a node as blocked when the scheduler would
// happily use it.
func TestFitTaints(t *testing.T) {
	spec := func(tols ...model.Toleration) model.PendingSpec {
		return model.PendingSpec{NeedCPUMilli: 100, NeedMemBytes: gi, Tolerations: tols}
	}
	big := func(opts ...func(*model.NodeCapacity)) []model.NodeCapacity {
		return []model.NodeCapacity{node("n", 8000, 32*gi, opts...)}
	}
	for _, tc := range []struct {
		name string
		spec model.PendingSpec
		node []model.NodeCapacity
		fits bool
	}{
		{"untolerated NoSchedule blocks", spec(), big(taint("gpu", "true", "NoSchedule")), false},
		{"untolerated NoExecute blocks", spec(), big(taint("gpu", "true", "NoExecute")), false},
		{"PreferNoSchedule is a preference, not a barrier", spec(), big(taint("spot", "", "PreferNoSchedule")), true},
		{"Equal toleration with matching value", spec(model.Toleration{Key: "gpu", Value: "true"}), big(taint("gpu", "true", "NoSchedule")), true},
		{"Equal toleration with wrong value", spec(model.Toleration{Key: "gpu", Value: "false"}), big(taint("gpu", "true", "NoSchedule")), false},
		{"Exists toleration on the key", spec(model.Toleration{Key: "gpu", Operator: "Exists"}), big(taint("gpu", "anything", "NoSchedule")), true},
		{"empty key with Exists tolerates everything", spec(model.Toleration{Operator: "Exists"}), big(taint("whatever", "x", "NoExecute")), true},
		{"effect-scoped toleration ignores other effects", spec(model.Toleration{Key: "gpu", Operator: "Exists", Effect: "NoExecute"}), big(taint("gpu", "x", "NoSchedule")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Fit(tc.spec, tc.node)[0].Fits; got != tc.fits {
				t.Errorf("fits = %v, want %v (%v)", got, tc.fits, Fit(tc.spec, tc.node)[0].Reasons)
			}
		})
	}
}

func TestFitNodeSelector(t *testing.T) {
	spec := model.PendingSpec{NeedCPUMilli: 100, NeedMemBytes: gi,
		NodeSelector: map[string]string{"disk": "ssd"}}
	fits := Fit(spec, []model.NodeCapacity{
		node("has-ssd", 8000, 32*gi, labelled("disk", "ssd")),
		node("has-hdd", 8000, 32*gi, labelled("disk", "hdd")),
		node("unlabelled", 8000, 32*gi),
	})
	byName := map[string]model.NodeFit{}
	for _, f := range fits {
		byName[f.Node] = f
	}
	if !byName["has-ssd"].Fits {
		t.Error("matching label should fit")
	}
	if !strings.Contains(byName["has-hdd"].Reasons[0], "node has hdd") {
		t.Errorf("mismatch should name the actual value: %v", byName["has-hdd"].Reasons)
	}
	if !strings.Contains(byName["unlabelled"].Reasons[0], "no such label") {
		t.Errorf("missing label should say so: %v", byName["unlabelled"].Reasons)
	}
}

// TestFitCordonedAndNotReady pins the ordering: a cordoned node is reported as
// cordoned, not as short of memory, even when both are true. The first reason is the
// one to act on.
func TestFitCordonedAndNotReady(t *testing.T) {
	spec := model.PendingSpec{NeedCPUMilli: 100, NeedMemBytes: gi}
	fits := Fit(spec, []model.NodeCapacity{
		node("cordoned", 8000, 32*gi, func(n *model.NodeCapacity) { n.Unschedulable = true }),
		node("down", 8000, 32*gi, func(n *model.NodeCapacity) { n.Ready = false }),
	})
	byName := map[string]model.NodeFit{}
	for _, f := range fits {
		byName[f.Node] = f
	}
	if !strings.Contains(byName["cordoned"].Reasons[0], "cordoned") {
		t.Errorf("want cordoned first: %v", byName["cordoned"].Reasons)
	}
	if !strings.Contains(byName["down"].Reasons[0], "not Ready") {
		t.Errorf("want not-Ready first: %v", byName["down"].Reasons)
	}
}

// TestSummarizeGroups checks the report stays an answer rather than becoming data.
// Forty nodes each with a line is not an explanation.
func TestSummarizeGroups(t *testing.T) {
	spec := model.PendingSpec{NeedCPUMilli: 100, NeedMemBytes: 30 * gi}
	var nodes []model.NodeCapacity
	for i := 0; i < 12; i++ {
		nodes = append(nodes, node(string(rune('a'+i)), 8000, 32*gi, used(0, 20*gi)))
	}
	for i := 0; i < 3; i++ {
		nodes = append(nodes, node("t"+string(rune('a'+i)), 8000, 32*gi, taint("gpu", "true", "NoSchedule")))
	}
	feasible, summary := Summarize(Fit(spec, nodes))

	if feasible != 0 {
		t.Errorf("feasible = %d, want 0", feasible)
	}
	joined := strings.Join(summary, "\n")
	if !strings.Contains(joined, "12 node(s): insufficient memory") {
		t.Errorf("memory bucket not grouped:\n%s", joined)
	}
	if !strings.Contains(joined, "3 node(s): untolerated taint gpu=true") {
		t.Errorf("taint bucket should keep which taint blocks:\n%s", joined)
	}
}

// TestNotCheckedIsHonest guards the property that separates a useful scheduling
// explanation from a misleading one: it must name what it did not evaluate.
func TestNotCheckedIsHonest(t *testing.T) {
	plain := NotChecked(model.PendingSpec{})
	if len(plain) == 0 {
		t.Fatal("NotChecked must never be empty; the scheduler weighs more than argus does")
	}
	withAffinity := NotChecked(model.PendingSpec{HasAffinity: true})
	if !strings.Contains(withAffinity[0], "nodeAffinity") {
		t.Errorf("a pod that declares nodeAffinity must be told it was not evaluated: %v", withAffinity[0])
	}
	if len(withAffinity) <= len(plain) {
		t.Error("the affinity warning should be additional, not a replacement")
	}
}

// TestFitIsDeterministic — the reason list is built from a label map, which is the
// exact shape that made event grouping flap before.
func TestFitIsDeterministic(t *testing.T) {
	spec := model.PendingSpec{NeedCPUMilli: 100, NeedMemBytes: gi,
		NodeSelector: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}}
	nodes := []model.NodeCapacity{node("n", 8000, 32*gi)}
	first := strings.Join(Fit(spec, nodes)[0].Reasons, "|")
	for i := 0; i < 25; i++ {
		if got := strings.Join(Fit(spec, nodes)[0].Reasons, "|"); got != first {
			t.Fatalf("run %d differs:\n  %s\n  %s", i, first, got)
		}
	}
}
