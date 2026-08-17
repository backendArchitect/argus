package kube

import (
	"context"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/backendArchitect/argus/internal/model"
	"github.com/backendArchitect/argus/internal/project"
)

// Triage answers "what is broken right now" across a whole cluster.
//
// It deliberately does NOT loop Gather. A per-workload gather costs ~13 apiserver calls, and a real
// cluster has hundreds of workloads — 165 on the cluster this was built against, which would be
// roughly 2,100 calls against a budget of 60, issued precisely when the control plane is already
// under stress during an incident.
//
// So the data flow is inverted: a fixed handful of cluster-wide list calls, then pods are grouped by
// owner locally and one synthetic Snapshot is assembled per owner. Cost is constant in the number of
// workloads. The detectors are reused unchanged, which is the whole reason they were written as pure
// functions over a Snapshot.
func (c *Client) Triage(ctx context.Context, namespace string) ([]*model.Snapshot, []string, []string, error) {
	ctx, cancel := c.WithTimeout(ctx)
	defer cancel()

	now := time.Now()
	var degraded, notes []string
	degrade := func(step string, err error) { degraded = append(degraded, fmt.Sprintf("%s: %v", step, err)) }

	// One list per kind, whatever the cluster size. An empty namespace means all namespaces.
	pods, err := c.Typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{Limit: 2000})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing pods: %w", err)
	}
	deploys, err := c.Typed.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{Limit: 1000})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing deployments: %w", err)
	}
	stses, err := c.Typed.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{Limit: 1000})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	dses, err := c.Typed.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{Limit: 1000})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing daemonsets: %w", err)
	}

	// The remainder are best-effort: without them some detectors lose corroboration and dock their
	// own confidence, which is better than failing the whole scan.
	var rsList []appsv1.ReplicaSet
	if l, err := c.Typed.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{Limit: 2000}); err != nil {
		degrade("replicasets", err)
	} else {
		rsList = l.Items
	}
	var events []corev1.Event
	if l, err := c.Typed.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{Limit: 2000}); err != nil {
		degrade("events", err)
	} else {
		events = l.Items
	}
	nodes := map[string]model.NodeView{}
	if l, err := c.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err != nil {
		degrade("nodes", err)
	} else {
		for i := range l.Items {
			nodes[l.Items[i].Name] = project.Node(&l.Items[i], now)
		}
	}
	usage := map[string]map[string]corev1.ResourceList{}
	if l, err := c.Metrics.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{}); err != nil {
		degrade("metrics", err)
	} else {
		for _, pm := range l.Items {
			byC := map[string]corev1.ResourceList{}
			for _, cm := range pm.Containers {
				byC[cm.Name] = cm.Usage
			}
			usage[pm.Namespace+"/"+pm.Name] = byC
		}
	}
	svcs, slices := c.triageServices(ctx, namespace, &degraded)

	notes = append(notes, "hpa and pdb are not gathered cluster-wide; no current detector reads them")

	// rsOwner maps a ReplicaSet to the Deployment above it, so a pod's owner chain resolves to the
	// controller a human actually thinks in terms of.
	rsOwner := map[string]string{}
	rsByOwner := map[string][]appsv1.ReplicaSet{}
	for i := range rsList {
		rs := &rsList[i]
		if o := metav1.GetControllerOf(rs); o != nil && o.Kind == "Deployment" {
			key := rs.Namespace + "/Deployment/" + o.Name
			rsOwner[rs.Namespace+"/"+rs.Name] = key
			rsByOwner[key] = append(rsByOwner[key], *rs)
		}
	}

	// Group pods by the controller two levels up where there is one.
	podsByOwner := map[string][]corev1.Pod{}
	for i := range pods.Items {
		p := &pods.Items[i]
		o := metav1.GetControllerOf(p)
		if o == nil {
			continue // a bare pod has no owner to group under; triage reports controllers
		}
		key := p.Namespace + "/" + o.Kind + "/" + o.Name
		if o.Kind == "ReplicaSet" {
			if up, ok := rsOwner[p.Namespace+"/"+o.Name]; ok {
				key = up
			}
		}
		podsByOwner[key] = append(podsByOwner[key], *p)
	}

	// Assemble one snapshot per controller.
	var out []*model.Snapshot
	idle := 0
	add := func(kind, name, ns string, w *model.WorkloadView, sel map[string]string) {
		key := ns + "/" + kind + "/" + name
		owned := podsByOwner[key]
		if len(owned) == 0 {
			// No pods means nothing for a pod-level detector to see. A scaled-to-zero workload is
			// deliberately idle, and the endpoint detector already refuses to report it.
			idle++
			return
		}
		snap := &model.Snapshot{
			Scope: "workload/" + ns + "/" + name, Namespace: ns, Workload: w,
			// Only Degraded is per-snapshot: detectors read it to dock confidence. Notes are about
			// the scan as a whole and are reported once by the caller, not repeated per workload.
			Degraded: degraded,
		}
		usedNodes := map[string]bool{}
		for i := range owned {
			p := &owned[i]
			snap.Pods = append(snap.Pods, project.Pod(p, usage[p.Namespace+"/"+p.Name], now))
			if p.Spec.NodeName != "" {
				usedNodes[p.Spec.NodeName] = true
			}
		}
		project.SortPods(snap.Pods)
		for n := range usedNodes {
			if nv, ok := nodes[n]; ok {
				snap.Nodes = append(snap.Nodes, nv)
			}
		}
		sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].Name < snap.Nodes[j].Name })

		for _, rs := range rsByOwner[key] {
			snap.ReplicaSets = append(snap.ReplicaSets, project.ReplicaSet(&rs, currentTemplateHash(rsByOwner[key]), now))
		}
		snap.Events = project.Events(eventsFor(events, ns, name), now)
		snap.Services = servicesFor(svcs, slices, ns, name, sel, owned)
		out = append(out, snap)
	}

	for i := range deploys.Items {
		d := &deploys.Items[i]
		w := project.Workload(d, now)
		add("Deployment", d.Name, d.Namespace, w, w.Selector)
	}
	for i := range stses.Items {
		s := &stses.Items[i]
		add("StatefulSet", s.Name, s.Namespace, statefulSetView(s, now), selectorOf(s.Spec.Selector))
	}
	for i := range dses.Items {
		d := &dses.Items[i]
		add("DaemonSet", d.Name, d.Namespace, daemonSetView(d, now), selectorOf(d.Spec.Selector))
	}

	// Say why the scanned count is lower than the workload count. Otherwise the difference reads as
	// workloads that were missed by an error rather than ones with nothing to look at.
	if idle > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d workload(s) have no pods (scaled to zero) and were not scanned; there is nothing for a "+
				"pod-level detector to see", idle))
	}
	if len(pods.Items) >= 2000 {
		notes = append(notes, "pod list hit the 2000 item cap; some workloads may not have been scanned")
	}
	return out, degraded, notes, nil
}

// triageServices lists services and endpoint slices once for the whole scope.
func (c *Client) triageServices(ctx context.Context, ns string, degraded *[]string) (
	[]corev1.Service, map[string][2]int) {

	counts := map[string][2]int{} // namespace/service -> {ready, notReady}
	l, err := c.Typed.CoreV1().Services(ns).List(ctx, metav1.ListOptions{Limit: 2000})
	if err != nil {
		*degraded = append(*degraded, fmt.Sprintf("services: %v", err))
		return nil, counts
	}
	sl, err := c.Typed.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{Limit: 4000})
	if err != nil {
		*degraded = append(*degraded, fmt.Sprintf("endpointslices: %v", err))
		return l.Items, counts
	}
	for _, s := range sl.Items {
		name := s.Labels[discoveryv1.LabelServiceName]
		if name == "" {
			continue
		}
		k := s.Namespace + "/" + name
		c := counts[k]
		for _, ep := range s.Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				c[0]++
			} else {
				c[1]++
			}
		}
		counts[k] = c
	}
	return l.Items, counts
}

// servicesFor picks the services fronting one workload, using the same subset-or-matching-name rule
// the per-workload gather uses so triage and diagnose agree about what belongs to what.
func servicesFor(svcs []corev1.Service, counts map[string][2]int, ns, workload string,
	sel map[string]string, pods []corev1.Pod) []model.ServiceView {

	var out []model.ServiceView
	for i := range svcs {
		s := &svcs[i]
		if s.Namespace != ns || len(s.Spec.Selector) == 0 {
			continue
		}
		if !relatedSelector(s.Spec.Selector, sel, s.Name, workload) {
			continue
		}
		matched := 0
		ls := labels.Set(s.Spec.Selector).AsSelector()
		for j := range pods {
			if ls.Matches(labels.Set(pods[j].Labels)) {
				matched++
			}
		}
		c := counts[ns+"/"+s.Name]
		out = append(out, project.Service(s, c[0], c[1], matched))
	}
	return out
}

// eventsFor filters the single cluster-wide event list down to one workload's object tree, using the
// same name-prefix rule as the per-workload gather.
func eventsFor(events []corev1.Event, ns, name string) []corev1.Event {
	out := make([]corev1.Event, 0, 8)
	for i := range events {
		e := &events[i]
		if e.Namespace != ns {
			continue
		}
		n := e.InvolvedObject.Name
		if n == name || (len(n) > len(name) && n[:len(name)] == name && n[len(name)] == '-') {
			out = append(out, *e)
		}
	}
	return out
}

func selectorOf(s *metav1.LabelSelector) map[string]string {
	if s == nil {
		return nil
	}
	return s.MatchLabels
}

func statefulSetView(s *appsv1.StatefulSet, now time.Time) *model.WorkloadView {
	w := &model.WorkloadView{
		Kind: "StatefulSet", Name: s.Name, Namespace: s.Namespace, Labels: s.Labels,
		Ready: s.Status.ReadyReplicas, Updated: s.Status.UpdatedReplicas,
		Available: s.Status.AvailableReplicas, Generation: s.Generation,
		ObservedGeneration: s.Status.ObservedGeneration,
		CreatedSecondsAgo:  int64(now.Sub(s.CreationTimestamp.Time).Seconds()),
		Selector:           selectorOf(s.Spec.Selector),
	}
	if s.Spec.Replicas != nil {
		w.Desired = *s.Spec.Replicas
	}
	return w
}

func daemonSetView(d *appsv1.DaemonSet, now time.Time) *model.WorkloadView {
	return &model.WorkloadView{
		Kind: "DaemonSet", Name: d.Name, Namespace: d.Namespace, Labels: d.Labels,
		Desired: d.Status.DesiredNumberScheduled, Ready: d.Status.NumberReady,
		Updated: d.Status.UpdatedNumberScheduled, Available: d.Status.NumberAvailable,
		Generation: d.Generation, ObservedGeneration: d.Status.ObservedGeneration,
		CreatedSecondsAgo: int64(now.Sub(d.CreationTimestamp.Time).Seconds()),
		Selector:          selectorOf(d.Spec.Selector),
	}
}
