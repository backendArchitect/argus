package kube

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/backendArchitect/argus/internal/model"
	"github.com/backendArchitect/argus/internal/project"
)

// sidecars are containers that are almost never the reason a workload is broken. Fetching
// istio-proxy's access log when the app container is OOMKilling is the single most common way to
// waste a round-trip and a context window during an incident.
var sidecars = map[string]bool{
	"istio-proxy": true, "istio-init": true, "linkerd-proxy": true, "linkerd-init": true,
	"envoy": true, "cloudsql-proxy": true, "cloud-sql-proxy": true, "vault-agent": true,
	"consul-sidecar": true, "dapr": true, "daprd": true, "otel-collector": true,
	"fluentbit": true, "fluent-bit": true, "filebeat": true, "datadog-agent": true,
	"gcp-auth-webhook": true, "sidecar": true,
}

// LogOptions controls log selection. Every field is optional; the zero value means "decide for me",
// which is the intended path.
type LogOptions struct {
	Container string // empty means auto-select
	Previous  *bool  // nil means decide from the container's state
	TailLines int64  // 0 means a sensible default
	SinceSecs int64  // 0 means no lower bound
	Budget    int    // token budget; 0 means the default
}

// defaultTailLines bounds what we ask the apiserver for. The token budget bounds what we emit, but
// an unbounded request can still stream hundreds of megabytes off a chatty container before we get
// the chance to trim it.
const defaultTailLines = 2000

// Logs fetches container output for a workload, choosing the pod, container and instance itself.
//
// The choices are the product. Anyone can call GetLogs; knowing that a crashlooping container's
// CURRENT instance has produced nothing and the interesting output is in the PREVIOUS one is the
// part that saves an incident's worth of thrash.
func (c *Client) Logs(ctx context.Context, ref Ref, opts LogOptions) (*model.LogBundle, error) {
	ctx, cancel := c.WithTimeout(ctx)
	defer cancel()

	sel, err := c.workloadSelector(ctx, ref)
	if err != nil {
		return nil, err
	}
	pods, err := c.Typed.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(sel).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods for %s: %w", ref, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("%s has no pods", ref)
	}

	pod := pickPod(pods.Items)
	container, previous, reason := pickContainer(pod, opts)

	tail := opts.TailLines
	if tail == 0 {
		tail = defaultTailLines
	}
	logOpts := &corev1.PodLogOptions{
		Container:  container,
		Previous:   previous,
		TailLines:  &tail,
		Timestamps: true,
	}
	if opts.SinceSecs > 0 {
		logOpts.SinceSeconds = &opts.SinceSecs
	}

	raw, err := c.readLogs(ctx, ref.Namespace, pod.Name, logOpts)
	if err != nil && previous {
		// A container that has never restarted has no previous instance, and the apiserver says so
		// with an error rather than an empty body. Fall back rather than failing the whole call.
		logOpts.Previous = false
		previous = false
		reason += "; no previous instance existed, so these are the current one's logs"
		raw, err = c.readLogs(ctx, ref.Namespace, pod.Name, logOpts)
	}
	if err != nil {
		return nil, err
	}

	groups, dropped := project.Logs(raw, time.Now(), opts.Budget)
	b := &model.LogBundle{
		Pod: pod.Name, Container: container, Previous: previous, Reason: reason,
		Groups: groups, DroppedGroups: dropped,
	}
	if dropped > 0 {
		b.Note = fmt.Sprintf("%d older distinct line(s) elided by the token budget; the newest are kept "+
			"because a failure appears at the end of a log, not the start", dropped)
	}
	if len(groups) == 0 {
		b.Note = "the container produced no output"
	}
	return b, nil
}

func (c *Client) readLogs(ctx context.Context, ns, pod string, o *corev1.PodLogOptions) (string, error) {
	stream, err := c.Typed.CoreV1().Pods(ns).GetLogs(pod, o).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("reading logs for %s/%s: %w", ns, pod, err)
	}
	defer stream.Close()

	// Cap the read. TailLines bounds the line count but not their size, and a single container
	// logging large JSON payloads can still stream far more than we will ever emit.
	const maxBytes = 4 << 20
	buf, err := io.ReadAll(io.LimitReader(stream, maxBytes))
	if err != nil {
		return "", fmt.Errorf("reading logs for %s/%s: %w", ns, pod, err)
	}
	return string(buf), nil
}

// pickPod chooses the pod most likely to explain the failure: least ready first, then most
// restarts. A healthy replica's logs say nothing about why its sibling is dying.
func pickPod(pods []corev1.Pod) *corev1.Pod {
	sorted := make([]*corev1.Pod, len(pods))
	for i := range pods {
		sorted[i] = &pods[i]
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := podReady(sorted[i]), podReady(sorted[j])
		if ri != rj {
			return !ri
		}
		return podRestarts(sorted[i]) > podRestarts(sorted[j])
	})
	return sorted[0]
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podRestarts(p *corev1.Pod) int32 {
	var n int32
	for _, cs := range p.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return n
}

// pickContainer selects the container worth reading and decides whether to read the previous
// instance. Returns the reason so the caller can show its working.
func pickContainer(pod *corev1.Pod, opts LogOptions) (name string, previous bool, reason string) {
	status := map[string]corev1.ContainerStatus{}
	for _, cs := range pod.Status.ContainerStatuses {
		status[cs.Name] = cs
	}

	if opts.Container != "" {
		name = opts.Container
		reason = "container was requested explicitly"
	} else {
		name, reason = interestingContainer(pod, status)
	}

	if opts.Previous != nil {
		return name, *opts.Previous, reason + "; previous was requested explicitly"
	}

	// The behaviour that saves the most time: on a crashlooping container the current instance is
	// in backoff and has written nothing, so its logs are empty and misleading. Whatever explains
	// the crash is in the instance that died.
	cs, ok := status[name]
	if ok && cs.LastTerminationState.Terminated != nil {
		waiting := cs.State.Waiting
		if waiting != nil && (waiting.Reason == "CrashLoopBackOff" || waiting.Reason == "Error") {
			return name, true, reason + fmt.Sprintf(
				"; reading the PREVIOUS instance because the container is %s and the current one has produced nothing (it last died with %s)",
				waiting.Reason, cs.LastTerminationState.Terminated.Reason)
		}
	}
	return name, false, reason
}

// interestingContainer prefers whatever is actually failing, and falls back to the first
// non-sidecar container.
func interestingContainer(pod *corev1.Pod, status map[string]corev1.ContainerStatus) (string, string) {
	var firstApp string
	for _, spec := range pod.Spec.Containers {
		if firstApp == "" && !sidecars[spec.Name] {
			firstApp = spec.Name
		}
		cs, ok := status[spec.Name]
		if !ok {
			continue
		}
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
			return spec.Name, fmt.Sprintf("container %q is the failing one (%s)", spec.Name, w.Reason)
		}
		if cs.LastTerminationState.Terminated != nil && cs.RestartCount > 0 {
			return spec.Name, fmt.Sprintf("container %q is the failing one (restarted %d times, last died with %s)",
				spec.Name, cs.RestartCount, cs.LastTerminationState.Terminated.Reason)
		}
		if !cs.Ready && !sidecars[spec.Name] {
			return spec.Name, fmt.Sprintf("container %q is not ready", spec.Name)
		}
	}
	if firstApp == "" && len(pod.Spec.Containers) > 0 {
		// Every container looked like a sidecar; the pod is probably a mesh component itself.
		return pod.Spec.Containers[0].Name, "no container is failing; using the only container"
	}
	if len(pod.Spec.Containers) > 1 {
		return firstApp, fmt.Sprintf("no container is failing; using %q, skipping sidecars", firstApp)
	}
	return firstApp, "no container is failing; using the only container"
}

// workloadSelector fetches just the pod selector, without the full gather a diagnosis needs.
func (c *Client) workloadSelector(ctx context.Context, ref Ref) (map[string]string, error) {
	switch ref.Kind {
	case "Deployment":
		d, err := c.Typed.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting %s: %w", ref, err)
		}
		return d.Spec.Selector.MatchLabels, nil
	case "StatefulSet":
		s, err := c.Typed.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting %s: %w", ref, err)
		}
		return s.Spec.Selector.MatchLabels, nil
	case "DaemonSet":
		d, err := c.Typed.AppsV1().DaemonSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting %s: %w", ref, err)
		}
		return d.Spec.Selector.MatchLabels, nil
	case "Rollout":
		u, err := c.Dynamic.Resource(rolloutGVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting %s: %w", ref, err)
		}
		m, _, _ := unstructuredMap(u.Object, "spec", "selector", "matchLabels")
		return m, nil
	}
	return nil, fmt.Errorf("unsupported kind %q", ref.Kind)
}

// RenderLogs formats a bundle for a human or a model to read.
func RenderLogs(b *model.LogBundle) string {
	var sb strings.Builder
	instance := "current"
	if b.Previous {
		instance = "PREVIOUS (the instance that died)"
	}
	fmt.Fprintf(&sb, "LOGS  pod/%s  container=%s  instance=%s\n", b.Pod, b.Container, instance)
	fmt.Fprintf(&sb, "why   %s\n", model.Wrap(b.Reason, 6))
	if b.Note != "" {
		fmt.Fprintf(&sb, "note  %s\n", model.Wrap(b.Note, 6))
	}
	sb.WriteString("\n")
	for _, g := range b.Groups {
		if g.Count > 1 {
			fmt.Fprintf(&sb, "[x%d] %s\n", g.Count, g.Text)
			continue
		}
		fmt.Fprintf(&sb, "     %s\n", g.Text)
	}
	return sb.String()
}
