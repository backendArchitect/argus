package detect

import (
	"fmt"
	"sort"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// blockingEffects are the taint effects that actually prevent scheduling.
// PreferNoSchedule is a preference, not a barrier, so reporting it as a reason a pod
// cannot schedule would be wrong — the scheduler will use such a node if it must.
var blockingEffects = map[string]bool{"NoSchedule": true, "NoExecute": true}

// Fit works out, node by node, whether a pending pod could be placed there and why
// not. Pure: no I/O, no clock, so it is testable against hand-written capacity
// tables, which matters because this is arithmetic and arithmetic is worth pinning.
//
// Order matters. A cordoned node is reported as cordoned, not as short of memory,
// even when both are true — the first reason is the one to act on, and listing five
// reasons per node across forty nodes is how a scheduling explanation becomes noise.
func Fit(spec model.PendingSpec, nodes []model.NodeCapacity) []model.NodeFit {
	out := make([]model.NodeFit, 0, len(nodes))
	for _, n := range nodes {
		f := model.NodeFit{
			Node:         n.Name,
			FreeCPUMilli: n.AllocCPUMilli - n.UsedCPUMilli,
			FreeMemBytes: n.AllocMemBytes - n.UsedMemBytes,
		}

		switch {
		case n.Unschedulable:
			f.Reasons = append(f.Reasons, "cordoned (spec.unschedulable)")
		case !n.Ready:
			f.Reasons = append(f.Reasons, "not Ready")
		}

		for _, t := range n.Taints {
			if !blockingEffects[t.Effect] || tolerates(spec.Tolerations, t) {
				continue
			}
			kv := t.Key
			if t.Value != "" {
				kv += "=" + t.Value
			}
			f.Reasons = append(f.Reasons, fmt.Sprintf("untolerated taint %s:%s", kv, t.Effect))
		}

		for _, want := range sortedPairs(spec.NodeSelector) {
			have, ok := n.Labels[want[0]]
			switch {
			case !ok:
				f.Reasons = append(f.Reasons, fmt.Sprintf(
					"nodeSelector wants %s=%s; node has no such label", want[0], want[1]))
			case have != want[1]:
				f.Reasons = append(f.Reasons, fmt.Sprintf(
					"nodeSelector wants %s=%s; node has %s", want[0], want[1], have))
			}
		}

		// The numbers are the point. "Insufficient memory" without them is a restatement
		// of the scheduler's own message, which the reader already has.
		if spec.NeedCPUMilli > 0 && f.FreeCPUMilli < spec.NeedCPUMilli {
			f.Reasons = append(f.Reasons, fmt.Sprintf(
				"insufficient cpu: needs %s, %s free of %s (%s already requested)",
				milli(spec.NeedCPUMilli), milli(f.FreeCPUMilli), milli(n.AllocCPUMilli), milli(n.UsedCPUMilli)))
		}
		if spec.NeedMemBytes > 0 && f.FreeMemBytes < spec.NeedMemBytes {
			f.Reasons = append(f.Reasons, fmt.Sprintf(
				"insufficient memory: needs %s, %s free of %s (%s already requested)",
				bytes(spec.NeedMemBytes), bytes(f.FreeMemBytes), bytes(n.AllocMemBytes), bytes(n.UsedMemBytes)))
		}

		f.Fits = len(f.Reasons) == 0
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Fits != out[j].Fits {
			return out[i].Fits // a node that fits is the useful one; show it first
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// tolerates implements Kubernetes' toleration matching for the fields that decide it.
func tolerates(tols []model.Toleration, t model.Taint) bool {
	for _, tol := range tols {
		if tol.Effect != "" && tol.Effect != t.Effect {
			continue
		}
		// An empty key with Exists tolerates everything, which is how DaemonSets and
		// cluster agents get scheduled onto tainted nodes.
		if tol.Key == "" {
			if tol.Operator == "Exists" {
				return true
			}
			continue
		}
		if tol.Key != t.Key {
			continue
		}
		if tol.Operator == "Exists" {
			return true
		}
		if tol.Value == t.Value { // Equal is the default operator
			return true
		}
	}
	return false
}

// Summarize groups the per-node verdicts into the two or three lines a reader
// actually needs. Forty nodes each with its own line is data, not an answer.
func Summarize(fits []model.NodeFit) (feasible int, summary []string) {
	buckets := map[string]int{}
	for _, f := range fits {
		if f.Fits {
			feasible++
			continue
		}
		// Bucket on the reason's category rather than its numbers, or every node lands
		// in its own bucket and the grouping achieves nothing.
		buckets[category(f.Reasons[0])]++
	}

	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if buckets[keys[i]] != buckets[keys[j]] {
			return buckets[keys[i]] > buckets[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		summary = append(summary, fmt.Sprintf("%d node(s): %s", buckets[k], k))
	}
	if feasible > 0 {
		summary = append([]string{fmt.Sprintf(
			"%d node(s) could take this pod — if it is still pending, the blocker is something "+
				"argus does not evaluate (see below)", feasible)}, summary...)
	}
	return feasible, summary
}

// category strips the numbers off a reason so equivalent reasons group together.
func category(reason string) string {
	for _, prefix := range []string{
		"insufficient cpu", "insufficient memory", "untolerated taint", "nodeSelector",
	} {
		if strings.HasPrefix(reason, prefix) {
			if prefix == "untolerated taint" {
				// Keep the taint itself: which taint is blocking is the actionable part.
				if i := strings.Index(reason, ":"); i > 0 {
					return reason[:strings.LastIndex(reason, ":")]
				}
			}
			return prefix
		}
	}
	return reason
}

// NotChecked states the limits of this analysis.
//
// The scheduler weighs more than argus does. Naming the gaps is the difference between
// a tool that helps and one that quietly misleads: a reader who believes every
// constraint was evaluated will stop looking in the right place.
func NotChecked(spec model.PendingSpec) []string {
	out := []string{
		"pod topology spread constraints",
		"inter-pod affinity and anti-affinity",
		"PersistentVolume zone or node affinity",
		"extended resources such as GPUs, and hugepages",
		"the maximum pods per node limit",
	}
	if spec.HasAffinity {
		// Worth its own line, because the pod demonstrably has one and we did not read it.
		out = append([]string{
			"this pod declares nodeAffinity, which argus does NOT evaluate — a node shown as " +
				"fitting may still be excluded by it",
		}, out...)
	}
	return out
}

func milli(m int64) string {
	if m%1000 == 0 {
		return fmt.Sprintf("%d", m/1000)
	}
	return fmt.Sprintf("%dm", m)
}

func bytes(b int64) string {
	const ki = 1024
	switch {
	case b < 0:
		return "-" + bytes(-b)
	case b >= ki*ki*ki:
		return fmt.Sprintf("%.1fGi", float64(b)/float64(ki*ki*ki))
	case b >= ki*ki:
		return fmt.Sprintf("%.0fMi", float64(b)/float64(ki*ki))
	default:
		return fmt.Sprintf("%dKi", b/ki)
	}
}

// sortedPairs returns a label map as key-sorted pairs.
//
// A SLICE, deliberately: the first version returned a map, so ranging over the result
// was just as unordered as ranging over the input and the function achieved nothing.
// Map order would make the reason list flap between runs — the same trap the event
// grouping hit.
func sortedPairs(m map[string]string) [][2]string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, m[k]})
	}
	return out
}

// MaxFitNodes bounds how many per-node lines are printed. On a large cluster the
// per-node detail is only useful for the nodes that came closest, so the rest are
// counted rather than listed.
const MaxFitNodes = 8

// RenderPending formats a scheduling explanation.
func RenderPending(reports []*model.PendingReport) string {
	var b strings.Builder
	if len(reports) == 0 {
		return "No pending pods. Every pod in this workload has been assigned to a node.\n"
	}

	for i, r := range reports {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "UNSCHEDULABLE  pod/%s  (pending %ds)\n", r.Pod, r.PendingSeconds)
		fmt.Fprintf(&b, "asked for      %s cpu, %s memory\n",
			milli(r.Spec.NeedCPUMilli), bytes(r.Spec.NeedMemBytes))
		if len(r.Spec.NodeSelector) > 0 {
			var parts []string
			for _, kv := range sortedPairs(r.Spec.NodeSelector) {
				parts = append(parts, kv[0]+"="+kv[1])
			}
			fmt.Fprintf(&b, "nodeSelector   %s\n", strings.Join(parts, ","))
		}
		if r.Reason != "" {
			fmt.Fprintf(&b, "scheduler      %s: %s\n", r.Reason, model.Wrap(r.Message, 15))
		}

		fmt.Fprintf(&b, "\nverdict        %d of %d node(s) could take it\n", r.Feasible, len(r.Nodes))
		for _, s := range r.Summary {
			fmt.Fprintf(&b, "  · %s\n", s)
		}

		shown := r.Nodes
		if len(shown) > MaxFitNodes {
			shown = shown[:MaxFitNodes]
		}
		b.WriteString("\nper node:\n")
		for _, f := range shown {
			if f.Fits {
				fmt.Fprintf(&b, "  ✓ %-34s fits (%s cpu, %s memory free)\n",
					f.Node, milli(f.FreeCPUMilli), bytes(f.FreeMemBytes))
				continue
			}
			fmt.Fprintf(&b, "  ✗ %-34s %s\n", f.Node, model.Wrap(f.Reasons[0], 39))
			for _, extra := range f.Reasons[1:] {
				fmt.Fprintf(&b, "    %-34s %s\n", "", model.Wrap(extra, 39))
			}
		}
		if len(r.Nodes) > len(shown) {
			fmt.Fprintf(&b, "  %d further node(s) not shown.\n", len(r.Nodes)-len(shown))
		}

		if len(r.NotChecked) > 0 {
			b.WriteString("\nnot evaluated by argus — if a node above looks like it should fit, the\n" +
				"answer is probably one of these:\n")
			for _, n := range r.NotChecked {
				fmt.Fprintf(&b, "  · %s\n", model.Wrap(n, 4))
			}
		}
	}
	return b.String()
}
