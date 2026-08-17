package detect

import (
	"fmt"

	"github.com/backendArchitect/argus/internal/model"
)

// tightProbeDeadline is the threshold below which a readiness probe's tolerance
// looks like a misconfiguration rather than a choice.
//
// Deadline is initialDelay + period*failureThreshold — the time a container has
// to start serving before the kubelet gives up on it. Almost no real service
// starts in under 10s; anything tighter is usually a copy-pasted default that
// nobody sized against the application.
const tightProbeDeadline = 10

// detectReadinessMisconfigured finds pods failing readiness while the process is
// alive and healthy.
//
// The distinction that matters: a container that is CRASHING is a different
// incident from one that is RUNNING but failing its probe. The first needs the
// crash fixed; the second usually needs the probe loosened. Conflating them
// sends people to read logs that contain nothing wrong.
func detectReadinessMisconfigured(s *model.Snapshot) []model.Finding {
	for i := range s.Pods {
		pod := &s.Pods[i]
		if pod.Ready {
			continue
		}
		for j := range pod.Containers {
			c := &pod.Containers[j]

			// The process must be alive. A waiting or terminated container is
			// crashing, which the OOM and rollout detectors own.
			if c.State.Status != "running" || c.Ready {
				continue
			}
			if c.Readiness == nil {
				continue
			}
			// Repeated restarts mean something IS killing it — that is a crash story,
			// not a probe-tuning story, even though the probe is also failing.
			if c.RestartCount > 2 {
				continue
			}

			deadline := c.Readiness.Deadline()
			alive := c.State.SecondsAgo // seconds since the container started

			// The container has been up considerably longer than the probe tolerates
			// and is still not ready. Either the app needs longer than the probe
			// allows, or the probe is pointed at the wrong port or path.
			if deadline > tightProbeDeadline || alive < int64(deadline) {
				continue
			}

			return []model.Finding{{
				ID:         "probe.readiness-misconfigured",
				Severity:   model.Warning,
				Confidence: confidence(s, 0.7),
				Scope:      workloadScope(s),
				Title: fmt.Sprintf("Container %q is running but has never passed readiness in %ds",
					c.Name, alive),
				Detail: fmt.Sprintf("The readiness probe allows only %ds before it gives up "+
					"(initialDelaySeconds %d + periodSeconds %d x failureThreshold %d), but the "+
					"container has been running for %ds without passing. The process is alive and "+
					"has restarted %d time(s), so this is not a crash — either the application "+
					"needs longer to start than the probe permits, or the probe is checking the "+
					"wrong %s target. Until it passes, the pod stays out of every Service.",
					deadline, c.Readiness.InitialDelay, c.Readiness.Period,
					c.Readiness.FailureThreshold, alive, c.RestartCount, c.Readiness.Kind),
				Evidence: []model.Evidence{
					evidence("pod.spec", "pod/"+pod.Name,
						"readiness probe: %s initialDelay=%ds period=%ds failureThreshold=%d timeout=%ds (deadline %ds)",
						c.Readiness.Kind, c.Readiness.InitialDelay, c.Readiness.Period,
						c.Readiness.FailureThreshold, c.Readiness.Timeout, deadline),
					evidence("pod.status", "pod/"+pod.Name,
						"container %q running for %ds, ready=false, restarts=%d",
						c.Name, alive, c.RestartCount),
				},
				NextTool: &model.ToolHint{
					Tool:   "get_workload_logs",
					Args:   map[string]string{"workload": pod.OwnerName},
					Reason: "the container's own logs show how long it actually takes to start serving, which is the number the probe should be sized against",
				},
			}}
		}
	}
	return nil
}
