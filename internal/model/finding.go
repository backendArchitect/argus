// Package model holds the plain-data types shared by the gather, detect and tool layers.
//
// Nothing here may reference a Kubernetes client, a context, or a live API object: Snapshot must
// round-trip through YAML so that `argus capture` writes fixtures, tests read them back, and
// production runs the identical detect path. That constraint is load-bearing — see the plan.
package model

// Severity is how bad the finding is if it is real. Ranked critical > warning > info.
type Severity string

const (
	Critical Severity = "critical"
	Warning  Severity = "warning"
	Info     Severity = "info"
)

// rank orders severities for sorting; higher is worse.
func (s Severity) rank() int {
	switch s {
	case Critical:
		return 3
	case Warning:
		return 2
	case Info:
		return 1
	}
	return 0
}

// Evidence is a citation. Every Finding must carry at least one — a high-confidence diagnosis with
// nothing to check is worse than no diagnosis during an incident.
type Evidence struct {
	Source  string `json:"source"`  // "pod.lastState", "event", "replicaset.diff", "metrics"
	Ref     string `json:"ref"`     // "pod/checkout-api-7d9f", "rs/checkout-api-7d9f"
	Excerpt string `json:"excerpt"` // the actual value that triggered the detector
}

// ToolHint points the model at the next useful call rather than making it guess.
type ToolHint struct {
	Tool   string            `json:"tool"`
	Args   map[string]string `json:"args,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

// Finding is one ranked diagnosis. Flat and JSON-friendly, matching the gospect-mcp finding model.
type Finding struct {
	ID       string   `json:"id"`       // "oomkill.limit-too-low"
	Severity Severity `json:"severity"` // critical | warning | info

	// Confidence is how sure we are the finding is real, independent of severity. Be honest: a
	// detector working from partial data (see Snapshot.Degraded) must dock its own confidence.
	Confidence float64 `json:"confidence"` // 0.0 - 1.0

	Scope    string     `json:"scope"`    // "workload/checkout-api", "node/gke-abc"
	Title    string     `json:"title"`    // one line
	Detail   string     `json:"detail"`   // 2-4 sentences: cause, not symptom
	Evidence []Evidence `json:"evidence"` // never empty; enforced by test
	NextTool *ToolHint  `json:"next_tool,omitempty"`

	// Suppresses lists finding IDs this one subsumes. The node detector uses it to widen scope:
	// three Deployments failing on one node is one node finding, not three workload diagnoses.
	Suppresses []string `json:"-"`
}

// Less reports whether f should sort before g: severity desc, then confidence desc, then ID for a
// stable order. Used with slices.SortStableFunc, so equal elements keep registry order.
func (f Finding) Less(g Finding) int {
	if d := g.Severity.rank() - f.Severity.rank(); d != 0 {
		return d
	}
	switch {
	case f.Confidence > g.Confidence:
		return -1
	case f.Confidence < g.Confidence:
		return 1
	}
	if f.ID < g.ID {
		return -1
	} else if f.ID > g.ID {
		return 1
	}
	return 0
}
