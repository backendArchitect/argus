package detect

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// exitCause maps a container exit code to what it actually means.
//
// This is the most common Kubernetes failure there is, and "CrashLoopBackOff, exit 1" is a symptom
// restated rather than a diagnosis. Several exit codes are genuinely self-explanatory, and each of
// those points at a completely different fix — a wrong entrypoint is not an application bug, and a
// container that exits zero is not failing at all.
type exitCause struct {
	id      string
	title   string
	detail  string
	certain bool // whether the code alone identifies the cause
}

// byExitCode covers the codes that mean something specific. Anything else is an application error
// whose reason lives only in the logs.
var byExitCode = map[int32]exitCause{
	0: {
		id:      "crashloop.exits-successfully",
		title:   "Container exits successfully but is restarted forever",
		detail:  "The container ran to completion and exited 0, and the controller restarted it because its restart policy is Always. Nothing is broken inside the container — the workload is the wrong shape. Work that finishes belongs in a Job or CronJob; a Deployment expects a process that stays up.",
		certain: true,
	},
	126: {
		id:      "crashloop.command-not-executable",
		title:   "Container command is not executable",
		detail:  "Exit 126 means the entrypoint was found but could not be run — typically a missing execute bit on a script, or a binary built for a different architecture than the node. This is an image or manifest problem, not an application bug, so the logs will be empty.",
		certain: true,
	},
	127: {
		id:      "crashloop.command-not-found",
		title:   "Container command was not found in the image",
		detail:  "Exit 127 means the shell or runtime could not find the entrypoint. Usually a typo in command/args, or a command that assumes a shell in an image that has none — distroless and scratch images have no /bin/sh, so shell-form commands fail this way. This is a manifest problem, not an application bug.",
		certain: true,
	},
	134: {
		id:      "crashloop.aborted",
		title:   "Container aborted (SIGABRT)",
		detail:  "Exit 134 is SIGABRT: the process called abort() or failed an assertion — a runtime panic, a failed C++ assert, or a JVM/Go runtime error. The stack trace leading up to it is in the previous instance's logs.",
		certain: true,
	},
	139: {
		id:      "crashloop.segfault",
		title:   "Container crashed with a segmentation fault (SIGSEGV)",
		detail:  "Exit 139 is SIGSEGV: the process accessed invalid memory. That is a bug in the binary or in a native dependency rather than a configuration problem, so rolling back to a working image is the fastest mitigation.",
		certain: true,
	},
	143: {
		id:      "crashloop.terminated-on-signal",
		title:   "Container keeps exiting on SIGTERM",
		detail:  "Exit 143 is SIGTERM. Something is asking the container to stop and it is being restarted — commonly a liveness probe killing a process that is alive but slow to answer, or an eviction. If a liveness probe is configured, check whether it is stricter than the application's real startup and response time.",
		certain: false,
	},
}

// startFailureReasons are terminated-state reasons meaning the container never ran at all. The
// container runtime, not the application, is reporting the problem.
//
// This matters because the intuitive mapping is wrong: a typo'd entrypoint does NOT produce exit
// 127. Verified on a live cluster — containerd fails to create the container and reports
// reason=StartError with exitCode 128, and the real cause sits in the termination message. Exit 127
// only appears when a shell actually ran and could not find the command. Treating 128 as an
// ordinary application error sent the reader to logs that cannot exist.
var startFailureReasons = map[string]bool{
	"StartError":           true,
	"CreateContainerError": true,
	"ContainerCannotRun":   true,
}

// execPath pulls the offending binary out of an OCI runtime error.
var execPath = regexp.MustCompile(`exec: "([^"]+)"`)

// detectCrashLoop finds containers that keep dying for a reason no other detector owns.
//
// Deliberately does NOT handle OOMKilled: that has its own detector with its own remedy (a limit
// change), and reporting both would be one cause stated twice. Image-pull failures never reach a
// terminated state at all, so there is no overlap there either.
func detectCrashLoop(s *model.Snapshot) []model.Finding {
	for i := range s.Pods {
		pod := &s.Pods[i]
		for j := range pod.Containers {
			c := &pod.Containers[j]
			if c.LastState == nil || c.LastState.Status != "terminated" {
				continue
			}
			// OOM is a different diagnosis with a different fix; detectOOMKill owns it.
			if c.LastState.Reason == "OOMKilled" {
				continue
			}
			// The container has to be failing now, not once upon a time.
			if staleFailure(c) {
				continue
			}
			// "Crash loop" means the container is looping NOW. A container that is running has
			// already come back, whatever it did earlier — and one that is running but not ready is
			// a readiness problem, which detectReadinessMisconfigured owns.
			//
			// This is a stricter rule than a time window, and it exists because a time window was
			// not enough: a kind node's containerd churned and restarted every pod with exit 255 /
			// reason=Unknown, and a 1-hour window then reported a healthy, serving workload as
			// crash-looping. Infrastructure churn is not an application diagnosis.
			if c.State.Status != "waiting" && c.State.Status != "terminated" {
				continue
			}
			// A single restart is not a loop either. Containers restart for benign reasons, and
			// reporting the first one is how a tool earns a reputation for crying wolf.
			if c.RestartCount < 2 {
				continue
			}

			cause, known := startFailureCause(c)
			if !known {
				cause, known = byExitCode[c.LastState.ExitCode]
			}
			if !known {
				cause = exitCause{
					id:    "crashloop.exiting-nonzero",
					title: fmt.Sprintf("Container %q is crash-looping, exiting %d", c.Name, c.LastState.ExitCode),
					detail: fmt.Sprintf("The container starts, exits %d (reason %q), and is restarted — "+
						"it has done so %d times. That exit code carries no standard meaning, so what went "+
						"wrong is only in the container's own output.",
						c.LastState.ExitCode, c.LastState.Reason, c.RestartCount),
				}
			}

			ev := []model.Evidence{
				evidence("pod.lastState", "pod/"+pod.Name,
					"container %q last terminated with reason=%s, exitCode=%d, %ds ago",
					c.Name, c.LastState.Reason, c.LastState.ExitCode, c.LastState.SecondsAgo),
				evidence("pod.status", "pod/"+pod.Name,
					"container %q has restarted %d times; current state is %s",
					c.Name, c.RestartCount, stateLabel(c)),
			}
			if msg := strings.TrimSpace(c.LastState.Message); msg != "" {
				ev = append(ev, evidence("pod.lastState", "pod/"+pod.Name, "termination message: %s", firstLine(msg)))
			}
			// For an entrypoint problem the command IS the finding, so cite it. It lives on the
			// ReplicaSet template rather than the pod, since it is a template-level field.
			if cmd := commandFor(s, c.Name); cmd != "" && cause.id != "crashloop.exiting-nonzero" && cause.id != "crashloop.exits-successfully" {
				ev = append(ev, evidence("replicaset.template", workloadScope(s),
					"container %q command is %s", c.Name, cmd))
			}
			if c.Liveness != nil && c.LastState.ExitCode == 143 {
				ev = append(ev, evidence("pod.spec", "pod/"+pod.Name,
					"liveness probe: %s initialDelay=%ds period=%ds failureThreshold=%d (deadline %ds)",
					c.Liveness.Kind, c.Liveness.InitialDelay, c.Liveness.Period,
					c.Liveness.FailureThreshold, c.Liveness.Deadline()))
			}

			// An exit code that identifies its own cause is worth claiming confidently. A bare
			// application error is not: we know it is crashing, not why.
			conf := 0.6
			if cause.certain {
				conf = 0.9
			}

			return []model.Finding{{
				ID:         cause.id,
				Severity:   model.Critical,
				Confidence: conf,
				Scope:      workloadScope(s),
				Title:      cause.title,
				Detail:     cause.detail,
				Evidence:   ev,
				NextTool: &model.ToolHint{
					Tool:   "get_workload_logs",
					Args:   map[string]string{"workload": pod.OwnerName, "previous": "true"},
					Reason: crashLogsReason(cause),
				},
			}}
		}
	}
	return nil
}

// crashLogsReason is honest about whether logs will help. For an entrypoint failure the container
// never started, so there is nothing to read — sending someone to the logs anyway wastes a call
// and, worse, makes an empty result look like a dead end rather than a clue.
func crashLogsReason(cause exitCause) string {
	switch cause.id {
	case "crashloop.container-wont-start", "crashloop.command-not-found", "crashloop.command-not-executable":
		return "the container never started, so expect no output — an empty log here CONFIRMS the " +
			"entrypoint never ran rather than ruling anything out"
	case "crashloop.exits-successfully":
		return "the previous instance's output shows what work it completed before exiting"
	default:
		return "the previous instance holds whatever it printed before dying; the current one is in backoff"
	}
}

// commandFor returns the container's entrypoint from the current ReplicaSet template.
func commandFor(s *model.Snapshot, container string) string {
	current, _ := currentRS(s)
	if current == nil {
		return ""
	}
	if spec := containerByName(current.Template, container); spec != nil {
		return strings.Join(append(append([]string{}, spec.Command...), spec.Args...), " ")
	}
	return ""
}

// startFailureCause reports a container the runtime could not start, naming the binary when the
// OCI error carries it.
func startFailureCause(c *model.ContainerView) (exitCause, bool) {
	if !startFailureReasons[c.LastState.Reason] {
		return exitCause{}, false
	}
	what := "its entrypoint could not be executed"
	if m := execPath.FindStringSubmatch(c.LastState.Message); len(m) == 2 {
		what = fmt.Sprintf("%q does not exist in the image, or is not executable", m[1])
	}
	return exitCause{
		id:    "crashloop.container-wont-start",
		title: fmt.Sprintf("Container %q cannot be started at all", c.Name),
		detail: fmt.Sprintf("The container runtime refused to create the container: %s. Nothing in "+
			"the application ran, so this is a manifest or image problem — a typo in command/args, a "+
			"binary missing from the image, a wrong architecture, or a shell-form command in an "+
			"image with no shell (distroless and scratch have no /bin/sh). The runtime's own error "+
			"is in the evidence below and is the whole diagnosis.", what),
		certain: true,
	}, true
}
