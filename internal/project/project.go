package project

import (
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/backendArchitect/argus/internal/model"
)

// Pod projects a pod and folds in its metrics, so a detector sees spec, status and usage together.
// usage is keyed by container name and may be nil when the metrics API is unavailable.
func Pod(p *corev1.Pod, usage map[string]corev1.ResourceList, now time.Time) model.PodView {
	v := model.PodView{
		Name:              p.Name,
		Node:              p.Spec.NodeName,
		Phase:             string(p.Status.Phase),
		Labels:            p.Labels,
		CreatedSecondsAgo: secondsAgo(now, p.CreationTimestamp.Time),
	}
	if o := metav1.GetControllerOf(p); o != nil {
		v.OwnerKind, v.OwnerName = o.Kind, o.Name
	}
	for _, c := range p.Status.Conditions {
		switch c.Type {
		case corev1.PodReady:
			v.Ready = c.Status == corev1.ConditionTrue
		case corev1.PodScheduled:
			if c.Status != corev1.ConditionTrue {
				v.SchedulingReason, v.SchedulingMessage = c.Reason, c.Message
			}
		}
	}

	// Index status by name: spec and status container order is not guaranteed to match.
	status := map[string]corev1.ContainerStatus{}
	for _, cs := range append(append([]corev1.ContainerStatus{},
		p.Status.ContainerStatuses...), p.Status.InitContainerStatuses...) {
		status[cs.Name] = cs
	}

	for i := range p.Spec.Containers {
		v.Containers = append(v.Containers, container(&p.Spec.Containers[i], status, usage, now))
	}
	return v
}

func container(c *corev1.Container, status map[string]corev1.ContainerStatus,
	usage map[string]corev1.ResourceList, now time.Time) model.ContainerView {

	spec := ContainerSpec(c)
	// Env keys, args and mounts are properties of the pod *template*, identical across every pod
	// of a ReplicaSet — repeating them per pod multiplied one real workload's 93 env keys by its
	// pod count and blew the per-pod budget by 2.4x on its own. They stay in ReplicaSetView.
	// .Template, which is the only place anything reads them (the rollout diff).
	spec.EnvKeys, spec.EnvFrom, spec.Args, spec.Command, spec.Mounts = nil, nil, nil, nil, nil
	cv := model.ContainerView{ContainerSpecView: spec}

	if cs, ok := status[c.Name]; ok {
		cv.Ready, cv.RestartCount = cs.Ready, cs.RestartCount
		if cs.Started != nil {
			cv.Started = *cs.Started
		}
		cv.State = state(cs.State, now)
		if s := state(cs.LastTerminationState, now); s.Status != "" {
			cv.LastState = &s
		}
	}
	if u, ok := usage[c.Name]; ok {
		if cpu := u.Cpu(); cpu != nil && !cpu.IsZero() {
			cv.UsageCPU = cpu.String()
		}
		if mem := u.Memory(); mem != nil && !mem.IsZero() {
			cv.UsageMem = mem.String()
		}
	}
	return cv
}

// ContainerSpec projects the desired shape of a container. Shared by Pod and by ReplicaSet
// template projection, which is what makes the rollout diff an apples-to-apples comparison.
func ContainerSpec(c *corev1.Container) model.ContainerSpecView {
	s := model.ContainerSpecView{
		Name:      c.Name,
		Image:     c.Image,
		Command:   c.Command,
		Args:      c.Args,
		Readiness: probe(c.ReadinessProbe),
		Liveness:  probe(c.LivenessProbe),
		Startup:   probe(c.StartupProbe),
	}
	if q := c.Resources.Requests.Cpu(); q != nil && !q.IsZero() {
		s.RequestCPU = q.String()
	}
	if q := c.Resources.Requests.Memory(); q != nil && !q.IsZero() {
		s.RequestMem = q.String()
	}
	if q := c.Resources.Limits.Cpu(); q != nil && !q.IsZero() {
		s.LimitCPU = q.String()
	}
	if q := c.Resources.Limits.Memory(); q != nil && !q.IsZero() {
		s.LimitMem = q.String()
	}

	// Env keys only. The values are the single most likely place for a credential to leak into
	// model context, and a key name is all any detector needs.
	for _, e := range c.Env {
		s.EnvKeys = append(s.EnvKeys, e.Name)
	}
	for _, f := range c.EnvFrom {
		switch {
		case f.ConfigMapRef != nil:
			s.EnvFrom = append(s.EnvFrom, "configmap/"+f.ConfigMapRef.Name)
		case f.SecretRef != nil:
			s.EnvFrom = append(s.EnvFrom, "secret/"+f.SecretRef.Name)
		}
	}
	for _, m := range c.VolumeMounts {
		// Skip the auto-injected SA token mount; it is on every pod and tells us nothing.
		if m.MountPath == "/var/run/secrets/kubernetes.io/serviceaccount" {
			continue
		}
		s.Mounts = append(s.Mounts, m.Name+":"+m.MountPath)
	}
	return s
}

func probe(p *corev1.Probe) *model.ProbeView {
	if p == nil {
		return nil
	}
	kind := "exec"
	switch {
	case p.HTTPGet != nil:
		kind = "http"
	case p.TCPSocket != nil:
		kind = "tcp"
	case p.GRPC != nil:
		kind = "grpc"
	}
	return &model.ProbeView{
		Kind:             kind,
		InitialDelay:     p.InitialDelaySeconds,
		Period:           p.PeriodSeconds,
		Timeout:          p.TimeoutSeconds,
		FailureThreshold: p.FailureThreshold,
		SuccessThreshold: p.SuccessThreshold,
	}
}

func state(s corev1.ContainerState, now time.Time) model.ContainerStateView {
	switch {
	case s.Running != nil:
		return model.ContainerStateView{
			Status: "running", SecondsAgo: secondsAgo(now, s.Running.StartedAt.Time)}
	case s.Waiting != nil:
		return model.ContainerStateView{
			Status: "waiting", Reason: s.Waiting.Reason, Message: s.Waiting.Message}
	case s.Terminated != nil:
		t := s.Terminated
		return model.ContainerStateView{
			Status: "terminated", Reason: t.Reason, Message: t.Message,
			ExitCode: t.ExitCode, Signal: t.Signal,
			SecondsAgo: secondsAgo(now, t.FinishedAt.Time),
		}
	}
	return model.ContainerStateView{}
}

// Workload projects a Deployment. StatefulSets and DaemonSets go through their own shims because
// their status fields have different names for the same concepts.
func Workload(d *appsv1.Deployment, now time.Time) *model.WorkloadView {
	w := &model.WorkloadView{
		Kind: "Deployment", Name: d.Name, Namespace: d.Namespace, Labels: d.Labels,
		Ready: d.Status.ReadyReplicas, Updated: d.Status.UpdatedReplicas,
		Available:          d.Status.AvailableReplicas,
		Generation:         d.Generation,
		ObservedGeneration: d.Status.ObservedGeneration,
		CreatedSecondsAgo:  secondsAgo(now, d.CreationTimestamp.Time),
	}
	if d.Spec.Replicas != nil {
		w.Desired = *d.Spec.Replicas
	}
	if d.Spec.Selector != nil {
		w.Selector = d.Spec.Selector.MatchLabels
	}
	for _, c := range d.Status.Conditions {
		w.Conditions = append(w.Conditions, model.ConditionView{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
			LastChangeSecsAgo: secondsAgo(now, c.LastTransitionTime.Time),
		})
	}
	return w
}

// ReplicaSet projects an RS plus its template, which is the input to the rollout diff.
func ReplicaSet(rs *appsv1.ReplicaSet, currentHash string, now time.Time) model.ReplicaSetView {
	v := model.ReplicaSetView{
		Name:              rs.Name,
		Revision:          rs.Annotations["deployment.kubernetes.io/revision"],
		Ready:             rs.Status.ReadyReplicas,
		Available:         rs.Status.AvailableReplicas,
		CreatedSecondsAgo: secondsAgo(now, rs.CreationTimestamp.Time),
		Current:           currentHash != "" && rs.Labels["pod-template-hash"] == currentHash,
	}
	if rs.Spec.Replicas != nil {
		v.Desired = *rs.Spec.Replicas
	}
	for i := range rs.Spec.Template.Spec.Containers {
		v.Template = append(v.Template, ContainerSpec(&rs.Spec.Template.Spec.Containers[i]))
	}
	return v
}

// Node projects the conditions that explain workload failures the workload did not cause.
func Node(n *corev1.Node, now time.Time) model.NodeView {
	v := model.NodeView{Name: n.Name, Unschedulable: n.Spec.Unschedulable}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			v.Ready = c.Status == corev1.ConditionTrue
		}
		// Only surface conditions that are firing; a healthy node's conditions are all noise.
		abnormal := (c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue) ||
			(c.Type != corev1.NodeReady && c.Status == corev1.ConditionTrue)
		if !abnormal {
			continue
		}
		v.Conditions = append(v.Conditions, model.ConditionView{
			Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message,
			LastChangeSecsAgo: secondsAgo(now, c.LastTransitionTime.Time),
		})
	}
	for _, t := range n.Spec.Taints {
		v.Taints = append(v.Taints, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
	}
	if q := n.Status.Allocatable.Cpu(); q != nil {
		v.AllocCPU = q.String()
	}
	if q := n.Status.Allocatable.Memory(); q != nil {
		v.AllocMem = q.String()
	}
	return v
}

// Service projects a Service together with the readiness of what it selects.
//
// ready/notReady come from EndpointSlices; matched is how many pods the selector hits at all.
// Keeping both is what lets the endpoint detector distinguish a label mismatch (matched == 0) from
// a readiness failure (matched > 0, ready == 0) — two very different incidents that look
// identical in `kubectl get svc`.
func Service(s *corev1.Service, ready, notReady, matched int) model.ServiceView {
	v := model.ServiceView{
		Name: s.Name, Selector: s.Spec.Selector,
		ReadyCount: ready, NotReadyCount: notReady, MatchedPods: matched,
	}
	for _, p := range s.Spec.Ports {
		v.Ports = append(v.Ports, fmt.Sprintf("%d/%s->%s", p.Port, p.Protocol, p.TargetPort.String()))
	}
	return v
}

// PDB projects a PodDisruptionBudget, which explains rollouts that are stuck rather than broken.
func PDB(p *policyv1.PodDisruptionBudget) *model.PDBView {
	return &model.PDBView{
		Name:               p.Name,
		DesiredHealthy:     p.Status.DesiredHealthy,
		CurrentHealthy:     p.Status.CurrentHealthy,
		DisruptionsAllowed: p.Status.DisruptionsAllowed,
	}
}

// SortPods orders pods worst-first so that a truncated list still shows what matters.
func SortPods(pods []model.PodView) {
	sort.SliceStable(pods, func(i, j int) bool {
		a, b := pods[i], pods[j]
		if a.Ready != b.Ready {
			return !a.Ready
		}
		return restarts(a) > restarts(b)
	})
}

func restarts(p model.PodView) int32 {
	var n int32
	for _, c := range p.Containers {
		n += c.RestartCount
	}
	return n
}
