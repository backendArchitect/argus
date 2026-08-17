// Package detect holds the correlation engine: pure functions over a
// model.Snapshot that produce ranked, evidence-backed findings.
//
// Every detector here must be a pure function. No I/O, no clock, no randomness —
// given the same Snapshot it must produce the same findings, or committed
// fixtures flap and the suite stops meaning anything. Purity is also what makes
// the tests clusterless and sub-second.
package detect

import (
	"fmt"
	"slices"

	"github.com/backendArchitect/argus/internal/model"
)

// Detector is one diagnosis rule.
type Detector struct {
	ID string
	// Detect returns zero or more findings. Returning nothing is the common and
	// correct case — a detector that always finds something is a detector nobody
	// will trust twice.
	Detect func(*model.Snapshot) []model.Finding
}

// registry is the detector set, in tie-break order. Sorting is stable, so two
// findings of equal severity and confidence keep this order.
var registry = []Detector{
	{ID: "node.unhealthy-host", Detect: detectNodePressure},
	{ID: "oomkill.limit-too-low", Detect: detectOOMKill},
	{ID: "rollout.bad-template", Detect: detectBadRollout},
	{ID: "image.pull-failed", Detect: detectImagePull},
	{ID: "endpoints.no-ready-backends", Detect: detectEndpointGap},
	{ID: "probe.readiness-misconfigured", Detect: detectReadinessMisconfigured},
}

// IDs returns the registered detector IDs, for tests and diagnostics.
func IDs() []string {
	out := make([]string, len(registry))
	for i, d := range registry {
		out[i] = d.ID
	}
	return out
}

// All runs every detector, applies scope-widening suppression, and ranks the
// result: severity descending, then confidence descending, then registry order.
func All(s *model.Snapshot) []model.Finding {
	var found []model.Finding
	for _, d := range registry {
		found = append(found, d.Detect(s)...)
	}

	// Scope widening: a finding may subsume others. If three unrelated Deployments
	// are failing on one node, the answer is the node — reporting each workload
	// separately sends three people to debug three symptoms of one cause.
	suppressed := map[string]bool{}
	for _, f := range found {
		for _, id := range f.Suppresses {
			suppressed[id] = true
		}
	}
	kept := found[:0]
	for _, f := range found {
		if !suppressed[f.ID] {
			kept = append(kept, f)
		}
	}

	slices.SortStableFunc(kept, func(a, b model.Finding) int { return a.Less(b) })
	return kept
}

// -- helpers shared by detectors ------------------------------------------------

// confidence caps a detector's confidence when the snapshot is missing the data
// the detector would have used to corroborate itself.
//
// This is the honesty rule made mechanical: "we could not reach the metrics API"
// must never render as "memory looks fine". A detector reasoning from absence
// says so by lowering its own number.
func confidence(s *model.Snapshot, base float64, needs ...string) float64 {
	for _, step := range needs {
		if s.Missing(step) {
			return base * 0.75
		}
	}
	return base
}

// evidence is a small constructor to keep detector bodies readable.
func evidence(source, ref, format string, args ...any) model.Evidence {
	return model.Evidence{Source: source, Ref: ref, Excerpt: fmt.Sprintf(format, args...)}
}

// workloadScope names the thing being diagnosed, for Finding.Scope.
func workloadScope(s *model.Snapshot) string {
	if s.Workload == nil {
		return s.Scope
	}
	return fmt.Sprintf("%s/%s/%s", s.Workload.Kind, s.Workload.Namespace, s.Workload.Name)
}

// currentRS returns the ReplicaSet the workload is currently rolling out to, and
// the newest one that is not current, or nil for either if absent.
func currentRS(s *model.Snapshot) (current, previous *model.ReplicaSetView) {
	for i := range s.ReplicaSets {
		rs := &s.ReplicaSets[i]
		switch {
		case rs.Current:
			current = rs
		case previous == nil || rs.CreatedSecondsAgo < previous.CreatedSecondsAgo:
			previous = rs
		}
	}
	return current, previous
}

// containerByName finds a container spec in a ReplicaSet template.
func containerByName(tmpl []model.ContainerSpecView, name string) *model.ContainerSpecView {
	for i := range tmpl {
		if tmpl[i].Name == name {
			return &tmpl[i]
		}
	}
	return nil
}
