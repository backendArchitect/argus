package detect

import (
	"fmt"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// signalKills are the exit codes that mean something outside the process stopped it: 137 is
// SIGKILL, 143 is SIGTERM. An application failing on its own reports its own code instead, which
// is what separates "the kubelet killed this" from "this crashed".
var signalKills = map[int32]bool{137: true, 143: true}

// detectStartupProbeTooShort finds a container the startup probe is killing before it can finish
// booting — the slow-starting application that looks exactly like a crash loop and is not one.
//
// The load-bearing fact is a rule of Kubernetes rather than a heuristic: **liveness and readiness
// probes do not run at all until the startup probe has succeeded.** So when a startup probe is
// configured and `started` is still false, no other probe can be the killer. Combined with a
// termination by signal, that narrows the cause to one thing, which is why this claims high
// confidence from a small number of fields.
//
// Before this detector existed the failure produced NOTHING, verified on a live cluster against a
// workload restarting every ~35 seconds forever. It fell between every existing gate:
//
//   - detectCrashLoop requires the container to be waiting or terminated *right now*, and this one
//     is usually caught running, partway through the boot window it never gets to finish.
//   - detectReadinessMisconfigured requires a readiness probe, and a workload can have a startup
//     probe without one.
//   - exit 137 is not in byExitCode, so even when the timing does catch it terminated, it lands on
//     the crashloop.exiting-nonzero fallback — "it is crashing, and only the logs say why". That is
//     the worst outcome of the three, because the logs show an application booting perfectly
//     normally and the reader spends the incident looking for a bug that is not there.
//
// It therefore suppresses both crash-loop findings it would otherwise be reported alongside. The
// container is not crashing; it is being killed, and saying both is saying one cause twice. The
// SIGTERM case matters most: that finding's advice is to look at the liveness probe, which cannot
// be the culprit here, because liveness does not run until startup passes.
//
// Known limitation, recorded rather than worked around: Suppresses is keyed by finding ID with no
// per-container scope, so in a multi-container pod where one container is held by its startup probe
// and a *different* one is genuinely crashing, the real crash-loop finding is suppressed too.
// Narrowing it would mean giving the suppression mechanism a scope it does not currently have, for
// a shape that is rare — a sidecar crashing while the app is still starting. Noted so the next
// person meets it as a documented trade-off rather than as a mystery.
func detectStartupProbeTooShort(s *model.Snapshot) []model.Finding {
	for i := range s.Pods {
		pod := &s.Pods[i]
		for j := range pod.Containers {
			c := &pod.Containers[j]
			if c.Startup == nil || c.Started {
				continue
			}
			// A single restart is not a loop, the same rule detectCrashLoop applies: containers
			// restart for benign reasons and reporting the first one is how a tool earns a
			// reputation for crying wolf.
			if c.RestartCount < 2 {
				continue
			}
			if c.LastState == nil || c.LastState.Status != "terminated" {
				continue
			}
			// OOM is a different diagnosis with a different fix, and detectOOMKill owns it. It
			// also shares exit code 137, which is exactly why that detector keys on the reason
			// rather than the code — and why this one has to exclude it explicitly.
			if c.LastState.Reason == "OOMKilled" {
				continue
			}
			if !signalKills[c.LastState.ExitCode] {
				continue
			}

			budget := c.Startup.Deadline()
			ev := []model.Evidence{
				evidence("pod.spec", "pod/"+pod.Name,
					"startup probe: %s initialDelay=%ds period=%ds failureThreshold=%d timeout=%ds "+
						"— a budget of %ds before the kubelet kills the container",
					c.Startup.Kind, c.Startup.InitialDelay, c.Startup.Period,
					c.Startup.FailureThreshold, c.Startup.Timeout, budget),
				evidence("pod.status", "pod/"+pod.Name,
					"container %q reports started=false after %d restarts; last terminated with "+
						"exitCode=%d (%s) %ds ago",
					c.Name, c.RestartCount, c.LastState.ExitCode, signalName(c.LastState.ExitCode),
					c.LastState.SecondsAgo),
			}

			// The probe's own complaint, when the event is still around. It carries the actual
			// failure text — a connection refused, a 404, a missing file — which distinguishes
			// "too slow" from "pointed at the wrong thing", and those have different fixes.
			conf := 0.8
			if g := startupProbeEvent(s); g != nil {
				ev = append(ev, evidence("event", g.ObjectKind+"/"+g.ObjectName,
					"%s x%d across %d pod(s): %s", g.Reason, g.Count, g.ObjectCount, g.Message))
				conf = 0.9
			}

			return []model.Finding{{
				ID:         "probe.startup-too-short",
				Severity:   model.Critical,
				Confidence: conf,
				Scope:      workloadScope(s),
				Title: fmt.Sprintf("The startup probe is killing container %q before it finishes starting",
					c.Name),
				Detail: fmt.Sprintf("The startup probe allows %ds (initialDelaySeconds %d + "+
					"periodSeconds %d x failureThreshold %d) for container %q to come up, and the "+
					"container has not managed it in %d attempts — it has never reported started=true "+
					"and is being killed by signal each time. This is not a crashing application: "+
					"liveness and readiness probes do not run until a startup probe passes, so with "+
					"started=false the startup probe is the only thing that can be killing it. Either "+
					"the application genuinely needs longer than %ds to boot, in which case raise "+
					"failureThreshold — that is what a startup probe is for, and a generous budget "+
					"costs nothing once it passes — or the probe is checking the wrong %s target and "+
					"would never pass however long it waited.",
					budget, c.Startup.InitialDelay, c.Startup.Period, c.Startup.FailureThreshold,
					c.Name, c.RestartCount, budget, c.Startup.Kind),
				Evidence: ev,
				// The crash-loop findings this would otherwise appear beside describe the symptom.
				Suppresses: []string{"crashloop.exiting-nonzero", "crashloop.terminated-on-signal"},
				NextTool: &model.ToolHint{
					Tool:   "get_workload_logs",
					Args:   map[string]string{"workload": pod.OwnerName, "previous": "true"},
					Reason: "the killed instance's logs show how far the application actually got before the probe gave up, which is the number to size the budget against",
				},
			}}
		}
	}
	return nil
}

// startupProbeEvent finds the kubelet's own report that the startup probe failed. Keyed on the
// message rather than the reason, because "Unhealthy" covers readiness and liveness failures too
// and only the message says which probe it was.
func startupProbeEvent(s *model.Snapshot) *model.EventGroup {
	for i := range s.Events {
		g := &s.Events[i]
		if g.Type == "Warning" && strings.Contains(g.Message, "Startup probe failed") {
			return g
		}
	}
	return nil
}

// signalName renders the exit code as the signal it encodes, so the evidence line reads as a cause
// rather than as a number the reader has to look up.
func signalName(code int32) string {
	switch code {
	case 137:
		return "SIGKILL"
	case 143:
		return "SIGTERM"
	}
	return "signal"
}
