package detect

import (
	"fmt"
	"sort"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// detectBadRollout finds workloads broken by their most recent deploy.
//
// "What changed" answers the majority of incidents, and the answer is sitting in
// the ReplicaSet history where nobody looks. The claim requires two halves: the
// current ReplicaSet is unhealthy, AND a previous one exists to compare against.
// Without the second half this is just "the workload is broken", which the user
// already knew.
func detectBadRollout(s *model.Snapshot) []model.Finding {
	current, previous := currentRS(s)
	if current == nil || previous == nil {
		return nil
	}
	// Healthy current rollout — nothing to say.
	if current.Desired > 0 && current.Ready >= current.Desired {
		return nil
	}
	// A brand-new rollout that has not had time to come up yet is not a bad
	// rollout. Without this, every deploy looks like an incident for its first
	// minute, and a tool that cries wolf during normal deploys gets muted.
	if current.CreatedSecondsAgo < 60 {
		return nil
	}

	diffs := diffTemplates(previous.Template, current.Template)
	if len(diffs) == 0 {
		return nil
	}

	ev := []model.Evidence{
		evidence("replicaset.status", "rs/"+current.Name,
			"current revision %s: %d/%d ready", current.Revision, current.Ready, current.Desired),
		evidence("replicaset.status", "rs/"+previous.Name,
			"previous revision %s: %d/%d ready, scaled to %d",
			previous.Revision, previous.Ready, previous.Desired, previous.Desired),
	}
	for _, d := range diffs {
		ev = append(ev, evidence("replicaset.diff", "rs/"+current.Name, "%s", d))
	}

	return []model.Finding{{
		ID:         "rollout.bad-template",
		Severity:   model.Critical,
		Confidence: confidence(s, 0.8, "replicasets"),
		Scope:      workloadScope(s),
		Title: fmt.Sprintf("Revision %s is failing; revision %s was healthy",
			current.Revision, previous.Revision),
		Detail: fmt.Sprintf("The current ReplicaSet %s has %d of %d replicas ready, and it "+
			"replaced %s. %d field(s) changed between the two revisions — the failure began "+
			"with this rollout, so the cause is almost certainly among them. Rolling back "+
			"restores the previous revision while you work out which.",
			current.Name, current.Ready, current.Desired, previous.Name, len(diffs)),
		Evidence: ev,
		NextTool: &model.ToolHint{
			Tool:   "get_workload_logs",
			Args:   map[string]string{"workload": nameOf(s), "previous": "true"},
			Reason: "the new revision's containers are failing; their previous logs show why",
		},
	}}
}

// diffTemplates reports semantic differences between two projected pod templates.
// Ordered so the fields that most often break a deploy come first — reading order
// is part of the diagnosis when a deploy changed several things at once.
func diffTemplates(prev, cur []model.ContainerSpecView) []string {
	var out []string

	for i := range cur {
		c := &cur[i]
		p := containerByName(prev, c.Name)
		if p == nil {
			out = append(out, fmt.Sprintf("container %q was added", c.Name))
			continue
		}
		add := func(field, was, now string) {
			if was != now {
				out = append(out, fmt.Sprintf("container %q %s: %s -> %s",
					c.Name, field, orNone(was), orNone(now)))
			}
		}
		add("memory limit", p.LimitMem, c.LimitMem)
		add("cpu limit", p.LimitCPU, c.LimitCPU)
		add("memory request", p.RequestMem, c.RequestMem)
		add("cpu request", p.RequestCPU, c.RequestCPU)
		add("image", p.Image, c.Image)
		add("args", strings.Join(p.Args, " "), strings.Join(c.Args, " "))
		add("readiness probe", probeSummary(p.Readiness), probeSummary(c.Readiness))
		add("liveness probe", probeSummary(p.Liveness), probeSummary(c.Liveness))

		if added, removed := diffSets(p.EnvKeys, c.EnvKeys); len(added)+len(removed) > 0 {
			out = append(out, fmt.Sprintf("container %q env keys: %s",
				c.Name, describeSetDiff(added, removed)))
		}
		if added, removed := diffSets(p.EnvFrom, c.EnvFrom); len(added)+len(removed) > 0 {
			out = append(out, fmt.Sprintf("container %q envFrom: %s",
				c.Name, describeSetDiff(added, removed)))
		}
		if added, removed := diffSets(p.Mounts, c.Mounts); len(added)+len(removed) > 0 {
			out = append(out, fmt.Sprintf("container %q mounts: %s",
				c.Name, describeSetDiff(added, removed)))
		}
	}
	for i := range prev {
		if containerByName(cur, prev[i].Name) == nil {
			out = append(out, fmt.Sprintf("container %q was removed", prev[i].Name))
		}
	}
	return out
}

func diffSets(prev, cur []string) (added, removed []string) {
	in := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	for _, c := range cur {
		if !in(prev, c) {
			added = append(added, c)
		}
	}
	for _, p := range prev {
		if !in(cur, p) {
			removed = append(removed, p)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func describeSetDiff(added, removed []string) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ","))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ","))
	}
	return strings.Join(parts, "; ")
}

func probeSummary(p *model.ProbeView) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%s delay=%ds period=%ds failures=%d",
		p.Kind, p.InitialDelay, p.Period, p.FailureThreshold)
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func nameOf(s *model.Snapshot) string {
	if s.Workload == nil {
		return ""
	}
	return s.Workload.Name
}
