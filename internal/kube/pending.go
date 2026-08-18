package kube

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/backendArchitect/argus/internal/detect"
	"github.com/backendArchitect/argus/internal/model"
)

// Pending explains why a workload's pods will not schedule.
//
// Needs data no other path gathers: every node's capacity, and the sum of requests
// already committed on each one. That second number is why this cannot reuse the
// workload gather — it requires every pod in the cluster, not just this workload's.
//
// Cost is four list calls regardless of cluster size.
func (c *Client) Pending(ctx context.Context, ref Ref) ([]*model.PendingReport, error) {
	ctx, cancel := c.WithTimeout(ctx)
	defer cancel()

	now := time.Now()
	sel, err := c.workloadSelector(ctx, ref)
	if err != nil {
		return nil, err
	}
	own, err := c.Typed.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(sel).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods for %s: %w", ref, err)
	}

	var pendingPods []corev1.Pod
	for i := range own.Items {
		p := &own.Items[i]
		if p.Status.Phase == corev1.PodPending && p.Spec.NodeName == "" {
			pendingPods = append(pendingPods, *p)
		}
	}
	if len(pendingPods) == 0 {
		return nil, nil
	}

	nodeList, err := c.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	// Every pod in the cluster, because what the scheduler reserves on a node is the sum
	// of the REQUESTS of everything already assigned there — not current usage, which is
	// a different number and the one people reach for by mistake.
	allPods, err := c.Typed.CoreV1().Pods("").List(ctx, metav1.ListOptions{Limit: 5000})
	if err != nil {
		return nil, fmt.Errorf("listing all pods to total committed requests: %w", err)
	}

	usedCPU, usedMem := map[string]int64{}, map[string]int64{}
	for i := range allPods.Items {
		p := &allPods.Items[i]
		// Terminal pods hold no reservation.
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, mem := podRequests(p)
		usedCPU[p.Spec.NodeName] += cpu
		usedMem[p.Spec.NodeName] += mem
	}

	caps := make([]model.NodeCapacity, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		nc := model.NodeCapacity{
			Name: n.Name, Labels: n.Labels, Unschedulable: n.Spec.Unschedulable,
			UsedCPUMilli: usedCPU[n.Name], UsedMemBytes: usedMem[n.Name],
		}
		if q := n.Status.Allocatable.Cpu(); q != nil {
			nc.AllocCPUMilli = q.MilliValue()
		}
		if q := n.Status.Allocatable.Memory(); q != nil {
			nc.AllocMemBytes = q.Value()
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				nc.Ready = c.Status == corev1.ConditionTrue
			}
		}
		for _, t := range n.Spec.Taints {
			nc.Taints = append(nc.Taints, model.Taint{Key: t.Key, Value: t.Value, Effect: string(t.Effect)})
		}
		caps = append(caps, nc)
	}

	var out []*model.PendingReport
	for i := range pendingPods {
		p := &pendingPods[i]
		cpu, mem := podRequests(p)
		spec := model.PendingSpec{
			NeedCPUMilli: cpu, NeedMemBytes: mem,
			NodeSelector: p.Spec.NodeSelector,
			HasAffinity:  p.Spec.Affinity != nil && p.Spec.Affinity.NodeAffinity != nil,
		}
		for _, t := range p.Spec.Tolerations {
			spec.Tolerations = append(spec.Tolerations, model.Toleration{
				Key: t.Key, Operator: string(t.Operator), Value: t.Value, Effect: string(t.Effect),
			})
		}

		r := &model.PendingReport{
			Pod: p.Name, Namespace: p.Namespace, Spec: spec,
			PendingSeconds: int64(now.Sub(p.CreationTimestamp.Time).Seconds()),
			NotChecked:     detect.NotChecked(spec),
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status != corev1.ConditionTrue {
				r.Reason, r.Message = cond.Reason, cond.Message
			}
		}
		r.Nodes = detect.Fit(spec, caps)
		r.Feasible, r.Summary = detect.Summarize(r.Nodes)
		out = append(out, r)
	}
	return out, nil
}

// podRequests totals a pod's resource requests the way the scheduler does: the sum of
// its containers, floored by the largest init container, since init containers run
// before the others and their peak has to fit too.
func podRequests(p *corev1.Pod) (cpuMilli, memBytes int64) {
	for i := range p.Spec.Containers {
		r := p.Spec.Containers[i].Resources.Requests
		if q := r.Cpu(); q != nil {
			cpuMilli += q.MilliValue()
		}
		if q := r.Memory(); q != nil {
			memBytes += q.Value()
		}
	}
	for i := range p.Spec.InitContainers {
		r := p.Spec.InitContainers[i].Resources.Requests
		if q := r.Cpu(); q != nil && q.MilliValue() > cpuMilli {
			cpuMilli = q.MilliValue()
		}
		if q := r.Memory(); q != nil && q.Value() > memBytes {
			memBytes = q.Value()
		}
	}
	return cpuMilli, memBytes
}
