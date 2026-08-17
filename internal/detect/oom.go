package detect

import (
	"fmt"

	"github.com/backendArchitect/argus/internal/model"
)

// oomHeadroom is the multiplier applied to observed peak usage when suggesting a
// new memory limit. 1.5x is a judgement call, not a law: enough slack that the
// workload survives a normal traffic spike, small enough that the suggestion is
// still a limit rather than an abdication.
const oomHeadroom = 1.5

// detectOOMKill finds containers the kernel killed for exceeding their memory
// limit.
//
// The signal is deliberately narrow: lastState.terminated.reason == OOMKilled.
// That field is set by the kubelet from the container runtime, and it is the one
// unambiguous statement that memory was the cause. Inferring OOM from exit code
// 137 alone would be wrong — 137 is just SIGKILL, which also covers liveness
// probe kills, evictions and manual deletes.
func detectOOMKill(s *model.Snapshot) []model.Finding {
	var out []model.Finding

	for i := range s.Pods {
		pod := &s.Pods[i]
		for j := range pod.Containers {
			c := &pod.Containers[j]
			if c.LastState == nil || c.LastState.Reason != "OOMKilled" {
				continue
			}

			if staleFailure(c) {
				continue
			}

			ev := []model.Evidence{
				evidence("pod.lastState", "pod/"+pod.Name,
					"container %q last terminated with reason=OOMKilled, exitCode=%d, %ds ago",
					c.Name, c.LastState.ExitCode, c.LastState.SecondsAgo),
			}

			limit := model.ParseMem(c.LimitMem)
			if c.LimitMem != "" {
				ev = append(ev, evidence("pod.spec", "pod/"+pod.Name,
					"container %q memory limit is %s", c.Name, c.LimitMem))
			}
			if c.RestartCount > 0 {
				ev = append(ev, evidence("pod.status", "pod/"+pod.Name,
					"container %q has restarted %d times; current state is %s",
					c.Name, c.RestartCount, stateLabel(c)))
			}

			detail, suggestion := oomDetail(c, limit)
			if suggestion != "" {
				ev = append(ev, evidence("metrics", "pod/"+pod.Name,
					"observed usage %s against limit %s", c.UsageMem, c.LimitMem))
			}

			// No memory limit set at all means the kill came from node pressure, not
			// from this container's own ceiling — a different problem with a different
			// fix, so say so rather than recommending a limit change.
			id := "oomkill.limit-too-low"
			title := fmt.Sprintf("Container %q in %s is being OOM-killed by its own memory limit",
				c.Name, pod.OwnerName)
			if limit == 0 {
				id = "oomkill.no-limit"
				title = fmt.Sprintf("Container %q was OOM-killed with no memory limit set", c.Name)
			}

			out = append(out, model.Finding{
				ID:       id,
				Severity: model.Critical,
				// Metrics corroborate the suggested new limit but are not needed to
				// establish the kill itself, so this stays high even when degraded.
				Confidence: confidence(s, 0.95, "metrics"),
				Scope:      workloadScope(s),
				Title:      title,
				Detail:     detail,
				Evidence:   ev,
				NextTool: &model.ToolHint{
					Tool:   "get_workload_logs",
					Args:   map[string]string{"workload": pod.OwnerName, "previous": "true"},
					Reason: logsHintReason(c),
				},
			})
			// One finding per pod is enough — every replica of a misconfigured
			// Deployment reports the identical fact, and repeating it per pod is the
			// per-pod noise cluster_triage exists to avoid.
			return out
		}
	}
	return out
}

// oomDetail writes the cause, and a limit suggestion when usage data supports one.
func oomDetail(c *model.ContainerView, limit int64) (detail, suggestion string) {
	base := fmt.Sprintf("The kernel killed container %q for exceeding its memory limit", c.Name)
	if limit == 0 {
		return base + ". No memory limit is set on the container, so this kill came from " +
			"node-level memory pressure rather than the container's own ceiling. Setting an " +
			"explicit limit and request would make the workload's needs schedulable and stop " +
			"it competing with its neighbours.", ""
	}

	switch {
	case c.State.Status == "waiting":
		detail = fmt.Sprintf("%s of %s. It is now in %s, so it will keep restarting and dying "+
			"until the limit is raised or its memory use comes down.", base, c.LimitMem, stateLabel(c))
	case c.Ready:
		detail = fmt.Sprintf("%s of %s. It has restarted and is serving again, but the kill was "+
			"recent enough to still be the incident: the limit will keep killing it under the "+
			"same load.", base, c.LimitMem)
	default:
		detail = fmt.Sprintf("%s of %s. It is %s and not yet ready, so it has not recovered from "+
			"the kill.", base, c.LimitMem, stateLabel(c))
	}

	usage := model.ParseMem(c.UsageMem)
	if usage == 0 {
		// Be explicit that the number is missing rather than silently omitting a
		// recommendation the reader might expect.
		return detail + " Current usage is unavailable (the metrics API did not answer), so " +
			"there is no measured basis for a new limit; size it from the workload's known " +
			"working set.", ""
	}

	suggestion = humanMem(int64(float64(usage) * oomHeadroom))
	return detail + fmt.Sprintf(" Observed usage reached %s against the %s limit; %s would give "+
		"roughly %.0f%% headroom.", c.UsageMem, c.LimitMem, suggestion, (oomHeadroom-1)*100), suggestion
}

func stateLabel(c *model.ContainerView) string {
	if c.State.Reason != "" {
		return c.State.Reason
	}
	return c.State.Status
}

// humanMem renders bytes as the Mi/Gi quantities Kubernetes manifests use.
func humanMem(b int64) string {
	const mi = 1024 * 1024
	if b >= 1024*mi {
		return fmt.Sprintf("%.1fGi", float64(b)/float64(1024*mi))
	}
	return fmt.Sprintf("%dMi", (b+mi-1)/mi)
}

// logsHintReason explains why the previous instance is the one to read. The wording has to match
// what the container is actually doing: a recovered container is not "in backoff", and telling an
// SRE otherwise sends them looking for a state that is not there.
func logsHintReason(c *model.ContainerView) string {
	if c.State.Status == "waiting" {
		return "the current container is in backoff and has produced nothing; the previous one holds " +
			"whatever it logged before the kill"
	}
	return "the current container restarted after the kill, so the output leading up to it is in the " +
		"previous instance"
}
