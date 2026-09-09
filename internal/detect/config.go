package detect

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// configCause classifies which config reference the kubelet could not resolve.
//
// All of these surface identically as CreateContainerConfigError and are fixed in different
// objects, which is the same reason detectImagePull splits ImagePullBackOff four ways: the reason
// string is a symptom, and the object to go edit is the diagnosis.
type configCause struct {
	id     string
	title  string
	detail string
}

// The kubelet's own wording, from the env-population path in kuberuntime. Verified verbatim
// against live clusters on Kubernetes 1.25.3 and 1.35.5 — identical on both:
//
//	configmap "nope-configmap" not found
//	secret "nope-secret" not found
//	couldn't find key ABSENT_KEY in ConfigMap argus-probe/real-cm
//	couldn't find key ABSENT_KEY in Secret argus-probe/real-secret
//
// Matched case-insensitively because the two paths disagree on capitalisation: the not-found
// message says "configmap", the missing-key message says "ConfigMap".
var (
	configMissingObject = regexp.MustCompile(`(?i)(configmap|secret) "([^"]+)" not found`)
	configMissingKey    = regexp.MustCompile(`(?i)couldn't find key (\S+) in (configmap|secret) (\S+)`)
)

// detectConfigError finds containers the kubelet cannot even construct, because a ConfigMap or
// Secret the pod references does not resolve.
//
// This is the failure with no logs. The container never starts, so there is no stream to read and
// no exit code to interpret — the usual "check the logs" reflex costs a round trip to find an
// empty stream, which is why this finding carries no log hint and says so instead.
//
// It is also why the crash-loop detector cannot see it: startFailureReasons there are
// *terminated*-state reasons, and this container never terminates. It sits in `waiting` with
// restartCount 0 for as long as the reference is broken, because the kubelet retries forever.
// Before this detector existed a workload in this state produced zero findings — argus reported
// nothing wrong about a Deployment that could never start a single pod.
//
// Deliberately NOT covered: the same missing object mounted as a VOLUME rather than read as env.
// That fails in the kubelet's mount path, reporting reason=ContainerCreating with a FailedMount
// event, and needs its own event-gated detector — ContainerCreating is the normal state for the
// first seconds of every pod's life, so it cannot be keyed on the way this reason can.
func detectConfigError(s *model.Snapshot) []model.Finding {
	for i := range s.Pods {
		pod := &s.Pods[i]
		for j := range pod.Containers {
			c := &pod.Containers[j]
			if c.State.Status != "waiting" || c.State.Reason != "CreateContainerConfigError" {
				continue
			}

			cause, object := classifyConfigError(c.State.Message)

			ev := []model.Evidence{
				evidence("pod.status", "pod/"+pod.Name,
					"container %q is %s: %s", c.Name, c.State.Reason, firstLine(c.State.Message)),
				// The proof that no logs exist, cited rather than asserted: a container that has
				// never started cannot have written anything. Both fields are read rather than
				// assumed — an evidence line that hardcodes the value it claims to have observed
				// is worth less than no evidence line, because it cannot be checked.
				evidence("pod.status", "pod/"+pod.Name,
					"container %q reports started=%t with %d restarts, so it has never run and "+
						"has produced no logs at all", c.Name, c.Started, c.RestartCount),
			}
			// Name where the reference is declared, when the template is in the snapshot. envFrom
			// lives only in ReplicaSetView.Template — the projection strips it from each pod to
			// stay inside the per-pod token budget — so this is absent for a StatefulSet or
			// DaemonSet, and the finding stands without it.
			if ref := envFromRef(s, c.Name, object); ref != "" {
				ev = append(ev, evidence("replicaset.template", ref,
					"container %q takes its environment from %s", c.Name, object))
			}

			detail := cause.detail
			if object != "" {
				detail = fmt.Sprintf("%s The reference that does not resolve is %s, in namespace %s.",
					detail, object, s.Namespace)
			}

			return []model.Finding{{
				ID:       cause.id,
				Severity: model.Critical,
				// No confidence() call, and deliberately: this detector reads the kubelet's own
				// verbatim statement of what is missing out of pod status, which is present
				// whenever pods are. It infers nothing from absence, so it has nothing to dock.
				Confidence: cause.confidence(),
				Scope:      workloadScope(s),
				Title:      cause.title,
				Detail: fmt.Sprintf("%s Container %q of %s cannot be created, so the pod will "+
					"never start and there are no logs to read.", detail, c.Name, pod.Name),
				Evidence: ev,
			}}
		}
	}
	return nil
}

// classifyConfigError reads the kubelet's message and says which object to go fix, plus that
// object as "kind/name" when the message named one.
//
// An ordered if-chain rather than the marker table detectImagePull uses: "not found" only means
// something paired with the kind that precedes it, and a flat list of OR'd substrings cannot
// express that pairing.
func classifyConfigError(msg string) (configCause, string) {
	// Bound the input before anything is parsed out of it. The kubelet's message is cluster
	// content, and the object name lands in Detail, which is rendered to a terminal and returned
	// to a model — so an embedded newline could forge what looks like a separate evidence line.
	// The apiserver's DNS-1123 validation makes that unreachable through the normal path today,
	// which is a reason to cap it here rather than to rely on it staying true. Every real message
	// is a single line, so the classifier loses nothing.
	msg = firstLine(msg)

	// Most specific first. A missing key is checked before a missing object because its message
	// also names a ConfigMap or Secret, and the object in that case exists.
	if m := configMissingKey.FindStringSubmatch(msg); m != nil {
		kind, display := configKind(m[2])
		return configCause{
			id:    "config.missing-key",
			title: "The " + display + " exists, but not the key the container asks for",
			detail: "This is the quiet one: the object is present, so `kubectl get` and any " +
				"reconciler watching it both look healthy, and it is a single key inside it " +
				"that the pod requires and the object does not define. Usually a key renamed " +
				"on one side of a deploy, or a typo in the env var's own reference.",
		}, kind + "/" + m[3] + " key " + m[1]
	}
	if m := configMissingObject.FindStringSubmatch(msg); m != nil {
		kind, display := configKind(m[1])
		cause := configCause{
			id:    "config.missing-" + kind,
			title: "The " + display + " the container needs does not exist",
			detail: "The pod requires a " + display + " that is not present in its namespace. " +
				"Either it was never applied, it was applied to a different namespace, or the " +
				"manifest names it with a typo. The kubelet retries forever, so the pod stays " +
				"here until the object appears — this will not resolve on its own.",
		}
		if kind == "secret" {
			// Worth its own sentence: unlike a ConfigMap, a Secret is very often not in the
			// manifest at all, and then the broken thing is the controller, not the reference.
			cause.detail += " Check whether the Secret is meant to be created by a controller " +
				"rather than by this manifest — sealed-secrets, external-secrets and the cloud " +
				"CSI drivers all work this way, and a healthy-looking reference plus a stalled " +
				"controller produces exactly this."
		}
		return cause, kind + "/" + m[2]
	}
	// Honest fallback: the config could not be built and the message did not name the object.
	// Better than guessing a ConfigMap and sending someone to check one that is perfectly fine.
	return configCause{
		id:    "config.missing-reference",
		title: "The kubelet cannot build the container's configuration",
		detail: "A reference in the container's environment or volumes does not resolve, and the " +
			"message does not name which one. The candidates are its env, envFrom and volume " +
			"references; each names a ConfigMap or Secret that must exist in this namespace.",
	}, ""
}

// configKind normalises the kind the kubelet named into the lowercase form used in finding IDs and
// object refs, and the canonical form used in prose.
//
// Two forms because they serve different readers: "configmap/checkout-config" must match the
// projection's env_from spelling so a reader can grep the snapshot for it, while a sentence about
// it has to say "ConfigMap", which is how the object is spelled everywhere else a Kubernetes user
// will look for it.
func configKind(matched string) (kind, display string) {
	if strings.EqualFold(matched, "secret") {
		return "secret", "Secret"
	}
	return "configmap", "ConfigMap"
}

// confidence is high for a classified cause and lower for the fallback, mirroring
// detectImagePull: a parsed object name is the kubelet quoting itself, while the fallback knows
// only that something did not resolve.
func (c configCause) confidence() float64 {
	if c.id == "config.missing-reference" {
		return 0.6
	}
	return 0.95
}

// envFromRef returns the ReplicaSet reference to cite when the current template's container really
// does declare the object the kubelet could not find, and "" otherwise.
//
// Gated on the template actually naming it, so the citation cannot claim a declaration the
// snapshot does not contain — a missing-key object is reached via env.valueFrom rather than
// envFrom, and would not appear here.
func envFromRef(s *model.Snapshot, container, object string) string {
	rs, _ := currentRS(s)
	if rs == nil || object == "" {
		return ""
	}
	spec := containerByName(rs.Template, container)
	if spec == nil {
		return ""
	}
	for _, e := range spec.EnvFrom {
		if e == object {
			return "rs/" + rs.Name
		}
	}
	return ""
}
