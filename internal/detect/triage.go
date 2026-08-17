package detect

import (
	"fmt"
	"slices"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// MaxTriageGroups bounds the report. A cluster with 40 broken workloads is having a bad day, and
// the top of that list is where anyone starts; the rest is counted, never silently dropped.
const MaxTriageGroups = 15

// Triage runs the detectors over every workload snapshot and assembles the cluster answer.
//
// The detectors are reused unchanged. That is the payoff of writing them as pure functions over a
// Snapshot: triage gets the same diagnoses as diagnose_workload, protected by the same fixtures,
// with no second implementation to drift.
func Triage(scope string, snaps []*model.Snapshot, degraded, notes []string) *model.TriageResult {
	res := &model.TriageResult{Scope: scope, Scanned: len(snaps), Degraded: degraded, Notes: notes}

	// Findings about shared infrastructure are hoisted out and deduplicated. Without this, one node
	// under memory pressure produces an identical critical finding on every workload it hosts —
	// the per-pod noise problem repeated at cluster scale, which is exactly what triage exists to
	// avoid.
	type clusterKey struct{ id, scope string }
	clusterSeen := map[clusterKey]int{}
	var clusterFindings []model.Finding

	for _, snap := range snaps {
		findings := All(snap)
		if len(findings) == 0 {
			continue
		}

		var own []model.Finding
		for _, f := range findings {
			if strings.HasPrefix(f.Scope, "node/") {
				k := clusterKey{f.ID, f.Scope}
				if _, ok := clusterSeen[k]; !ok {
					clusterFindings = append(clusterFindings, f)
				}
				clusterSeen[k]++
				continue
			}
			own = append(own, f)
		}
		if len(own) == 0 {
			continue
		}

		g := model.TriageGroup{Pods: len(snap.Pods), Findings: own}
		if w := snap.Workload; w != nil {
			g.Kind, g.Name, g.Namespace = w.Kind, w.Name, w.Namespace
			g.Ready, g.Desired = w.Ready, w.Desired
		}
		res.Groups = append(res.Groups, g)
	}

	// Say how many workloads each shared finding actually affects — that count is the difference
	// between "a node is unwell" and "a node is taking out half the cluster".
	for i := range clusterFindings {
		f := &clusterFindings[i]
		n := clusterSeen[clusterKey{f.ID, f.Scope}]
		f.Evidence = append(f.Evidence, evidence("triage", f.Scope,
			"%d workload(s) in this scan have pods on it", n))
	}
	res.Cluster = clusterFindings
	res.Unhealthy = len(res.Groups)

	// Worst first, by the severest finding in each group, then by how much of the workload is down.
	slices.SortStableFunc(res.Groups, func(a, b model.TriageGroup) int {
		if d := a.Findings[0].Less(b.Findings[0]); d != 0 {
			return d
		}
		if a.Desired != 0 && b.Desired != 0 {
			// A workload with nothing ready outranks one that is merely degraded.
			ar, br := float64(a.Ready)/float64(a.Desired), float64(b.Ready)/float64(b.Desired)
			if ar != br {
				if ar < br {
					return -1
				}
				return 1
			}
		}
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	if len(res.Groups) > MaxTriageGroups {
		res.Omitted = len(res.Groups) - MaxTriageGroups
		res.Groups = res.Groups[:MaxTriageGroups]
	}
	return res
}

// RenderTriage formats the cluster answer worst-first.
func RenderTriage(r *model.TriageResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TRIAGE  %s\n", r.Scope)
	fmt.Fprintf(&b, "scanned %d workload(s); %d with findings\n", r.Scanned, r.Unhealthy)

	if len(r.Cluster) == 0 && len(r.Groups) == 0 {
		b.WriteString("\nNothing matched. argus checked " + strings.Join(IDs(), ", ") + ".\n\n" +
			"That is not the same as \"the cluster is healthy\" — it means none of the detectors " +
			"above matched any workload. A problem outside what argus looks for will not appear here.\n")
		writeTriageGaps(&b, r)
		return b.String()
	}

	if len(r.Cluster) > 0 {
		b.WriteString("\n── infrastructure ──────────────────────────────────────────────\n")
		b.WriteString("Reported once here rather than repeated on every workload affected.\n")
		for _, f := range r.Cluster {
			fmt.Fprintf(&b, "\n[%s · %.0f%%] %s\n   %s\n", f.Severity, f.Confidence*100, f.ID, f.Title)
			for _, e := range f.Evidence {
				fmt.Fprintf(&b, "     · %s (%s):\n         %s\n", e.Ref, e.Source, model.Wrap(e.Excerpt, 9))
			}
		}
	}

	if len(r.Groups) > 0 {
		b.WriteString("\n── workloads ───────────────────────────────────────────────────\n")
		for _, g := range r.Groups {
			fmt.Fprintf(&b, "\n%s %s/%s  (%d/%d ready, %d pod(s))\n",
				g.Kind, g.Namespace, g.Name, g.Ready, g.Desired, g.Pods)
			for _, f := range g.Findings {
				fmt.Fprintf(&b, "  [%s · %.0f%%] %-34s %s\n",
					f.Severity, f.Confidence*100, f.ID, model.Wrap(f.Title, 4))
			}
		}
		fmt.Fprintf(&b, "\nRun 'diagnose_workload' on any of these for the evidence and the next step.\n")
	}
	if r.Omitted > 0 {
		fmt.Fprintf(&b, "%d further workload(s) with findings not shown.\n", r.Omitted)
	}
	writeTriageGaps(&b, r)
	return b.String()
}

func writeTriageGaps(b *strings.Builder, r *model.TriageResult) {
	if len(r.Degraded) > 0 {
		b.WriteString("\nincomplete — these lookups failed, and any finding relying on them has " +
			"had its confidence reduced:\n")
		for _, d := range r.Degraded {
			fmt.Fprintf(b, "  · %s\n", d)
		}
	}
	if len(r.Notes) > 0 {
		b.WriteString("\nelided deliberately:\n")
		for _, n := range r.Notes {
			fmt.Fprintf(b, "  · %s\n", n)
		}
	}
}
