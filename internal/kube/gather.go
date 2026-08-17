package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/backendArchitect/argus/internal/model"
	"github.com/backendArchitect/argus/internal/project"
)

// gatherFanout bounds concurrent apiserver calls. The call budget in client.go caps the total; this
// caps the instantaneous rate so a diagnosis never looks like a burst of load to a struggling
// control plane.
const (
	gatherFanout = 8

	// keepReplicaSets bounds rollout history. The diff only needs the current RS and the last
	// healthy one; a three-year-old Deployment can carry dozens, each with a full pod template.
	keepReplicaSets = 3
)

// Gather collects everything needed to diagnose one workload and projects it into a Snapshot.
//
// Failures are recorded in Snapshot.Degraded rather than returned. During an incident a partial
// diagnosis beats no diagnosis — but detectors must consult Snapshot.Missing and dock their
// confidence, so "we could not see the metrics API" never masquerades as "memory looks fine".
func (c *Client) Gather(ctx context.Context, ref Ref) (*model.Snapshot, error) {
	ctx, cancel := c.WithTimeout(ctx)
	defer cancel()

	now := time.Now()
	snap := &model.Snapshot{
		Scope:     "workload/" + ref.Namespace + "/" + ref.Name,
		Namespace: ref.Namespace,
	}

	var mu sync.Mutex
	degrade := func(step string, err error) {
		mu.Lock()
		defer mu.Unlock()
		snap.Degraded = append(snap.Degraded, fmt.Sprintf("%s: %v", step, err))
	}

	// Phase 1: the workload itself. Without it there is nothing to select pods by, so this is the
	// one failure that is fatal.
	sel, err := c.workload(ctx, ref, snap, now)
	if err != nil {
		return nil, err
	}

	// Phase 2: pods. Most of the remaining steps key off which pods and nodes are involved.
	pods, err := c.Typed.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(sel).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods for %s: %w", ref, err)
	}

	// Phase 3: everything else, concurrently. Each step owns its own degrade path.
	usage := map[string]map[string]corev1.ResourceList{} // pod -> container -> usage
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(gatherFanout)

	g.Go(func() error {
		u, err := c.metrics(gctx, ref.Namespace, pods.Items)
		if err != nil {
			degrade("metrics", err)
			return nil
		}
		mu.Lock()
		usage = u
		mu.Unlock()
		return nil
	})
	g.Go(func() error {
		rs, note, err := c.replicaSets(gctx, ref, sel, now)
		if err != nil {
			degrade("replicasets", err)
			return nil
		}
		mu.Lock()
		snap.ReplicaSets = rs
		if note != "" {
			snap.Notes = append(snap.Notes, note)
		}
		mu.Unlock()
		return nil
	})
	g.Go(func() error {
		evs, err := c.events(gctx, ref, now)
		if err != nil {
			degrade("events", err)
			return nil
		}
		mu.Lock()
		snap.Events = evs
		mu.Unlock()
		return nil
	})
	g.Go(func() error {
		svcs, err := c.services(gctx, ref.Namespace, ref.Name, sel, pods.Items)
		if err != nil {
			degrade("services", err)
			return nil
		}
		mu.Lock()
		snap.Services = svcs
		mu.Unlock()
		return nil
	})
	g.Go(func() error {
		nodes, err := c.nodes(gctx, pods.Items, now)
		if err != nil {
			degrade("nodes", err)
			return nil
		}
		mu.Lock()
		snap.Nodes = nodes
		mu.Unlock()
		return nil
	})
	g.Go(func() error {
		h, err := c.hpa(gctx, ref, now)
		if err != nil {
			degrade("hpa", err)
			return nil
		}
		mu.Lock()
		snap.HPA = h
		mu.Unlock()
		return nil
	})
	g.Go(func() error {
		p, err := c.pdb(gctx, ref.Namespace, ref.Name, sel)
		if err != nil {
			degrade("pdb", err)
			return nil
		}
		mu.Lock()
		snap.PDB = p
		mu.Unlock()
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Pods are projected last so they can fold in the metrics gathered above.
	for i := range pods.Items {
		snap.Pods = append(snap.Pods, project.Pod(&pods.Items[i], usage[pods.Items[i].Name], now))
	}
	project.SortPods(snap.Pods)
	return snap, nil
}

// workload fetches the controller and returns its pod selector and current pod-template hash.
func (c *Client) workload(ctx context.Context, ref Ref, snap *model.Snapshot, now time.Time) (
	sel map[string]string, err error) {

	switch ref.Kind {
	case "Deployment":
		d, err := c.Typed.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting %s: %w", ref, err)
		}
		snap.Workload = project.Workload(d, now)
		return snap.Workload.Selector, nil

	case "StatefulSet":
		s, err := c.Typed.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting %s: %w", ref, err)
		}
		w := &model.WorkloadView{
			Kind: "StatefulSet", Name: s.Name, Namespace: s.Namespace, Labels: s.Labels,
			Ready: s.Status.ReadyReplicas, Updated: s.Status.UpdatedReplicas,
			Available: s.Status.AvailableReplicas, Generation: s.Generation,
			ObservedGeneration: s.Status.ObservedGeneration,
			CreatedSecondsAgo:  int64(now.Sub(s.CreationTimestamp.Time).Seconds()),
		}
		if s.Spec.Replicas != nil {
			w.Desired = *s.Spec.Replicas
		}
		if s.Spec.Selector != nil {
			w.Selector = s.Spec.Selector.MatchLabels
		}
		snap.Workload = w
		return w.Selector, nil

	case "DaemonSet":
		d, err := c.Typed.AppsV1().DaemonSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting %s: %w", ref, err)
		}
		w := &model.WorkloadView{
			Kind: "DaemonSet", Name: d.Name, Namespace: d.Namespace, Labels: d.Labels,
			Desired: d.Status.DesiredNumberScheduled, Ready: d.Status.NumberReady,
			Updated: d.Status.UpdatedNumberScheduled, Available: d.Status.NumberAvailable,
			Generation: d.Generation, ObservedGeneration: d.Status.ObservedGeneration,
			CreatedSecondsAgo: int64(now.Sub(d.CreationTimestamp.Time).Seconds()),
		}
		if d.Spec.Selector != nil {
			w.Selector = d.Spec.Selector.MatchLabels
		}
		snap.Workload = w
		return w.Selector, nil

	case "Rollout":
		u, err := c.Dynamic.Resource(rolloutGVR).Namespace(ref.Namespace).
			Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting %s: %w", ref, err)
		}
		// ponytail: Rollout status is read through unstructured rather than importing the Argo API
		// for four fields. Revisit if Rollout support grows past this.
		w := &model.WorkloadView{Kind: "Rollout", Name: u.GetName(), Namespace: u.GetNamespace(),
			Labels: u.GetLabels(), Generation: u.GetGeneration(),
			CreatedSecondsAgo: int64(now.Sub(u.GetCreationTimestamp().Time).Seconds())}
		if m, ok, _ := unstructuredMap(u.Object, "spec", "selector", "matchLabels"); ok {
			w.Selector = m
		}
		snap.Workload = w
		return w.Selector, nil
	}
	return nil, fmt.Errorf("unsupported kind %q", ref.Kind)
}

// replicaSets returns the workload's ReplicaSets, marking the current one. Only Deployments own
// ReplicaSets; other kinds return nothing, which is not a degradation.
func (c *Client) replicaSets(ctx context.Context, ref Ref, sel map[string]string,
	now time.Time) ([]model.ReplicaSetView, string, error) {

	if ref.Kind != "Deployment" {
		return nil, "", nil
	}
	l, err := c.Typed.AppsV1().ReplicaSets(ref.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(sel).String(),
	})
	if err != nil {
		return nil, "", err
	}

	// The current RS is the newest one owned by this Deployment that still has desired replicas;
	// falling back to newest overall covers a scaled-to-zero rollout.
	var owned []appsv1.ReplicaSet
	for _, rs := range l.Items {
		if o := metav1.GetControllerOf(&rs); o != nil && o.Kind == "Deployment" && o.Name == ref.Name {
			owned = append(owned, rs)
		}
	}
	var currentHash string
	var newest *appsv1.ReplicaSet
	for i := range owned {
		rs := &owned[i]
		if newest == nil || rs.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = rs
		}
	}
	if newest != nil {
		currentHash = newest.Labels["pod-template-hash"]
	}

	// Keep the current RS plus the most recent others. A long-lived Deployment accumulates dozens
	// of ReplicaSets, each carrying a full pod template; the rollout diff only ever compares the
	// current one against the last that was healthy, so older history is pure context cost.
	sort.SliceStable(owned, func(i, j int) bool {
		return owned[i].CreationTimestamp.After(owned[j].CreationTimestamp.Time)
	})
	kept := owned
	if len(owned) > keepReplicaSets {
		kept = owned[:keepReplicaSets]
	}

	out := make([]model.ReplicaSetView, 0, len(kept))
	for i := range kept {
		out = append(out, project.ReplicaSet(&kept[i], currentHash, now))
	}
	var note string
	if len(kept) < len(owned) {
		note = fmt.Sprintf("replicasets: kept the %d newest of %d", len(kept), len(owned))
	}
	return out, note, nil
}

// events lists namespace events and keeps only those about this workload's object tree.
//
// Filtering happens on the raw events, before dedup: after grouping, ObjectName is only one
// example of many, so filtering on it there would drop whole groups because their representative
// happened to belong to a neighbouring workload.
//
// The name-prefix rule covers the whole tree without a second lookup — a Deployment's ReplicaSets
// are "<name>-<hash>", its pods "<name>-<hash>-<suffix>", a StatefulSet's pods "<name>-<ordinal>".
func (c *Client) events(ctx context.Context, ref Ref, now time.Time) ([]model.EventGroup, error) {
	l, err := c.Typed.CoreV1().Events(ref.Namespace).List(ctx, metav1.ListOptions{Limit: 500})
	if err != nil {
		return nil, err
	}
	ours := make([]corev1.Event, 0, len(l.Items))
	for _, e := range l.Items {
		n := e.InvolvedObject.Name
		if n == ref.Name || strings.HasPrefix(n, ref.Name+"-") {
			ours = append(ours, e)
		}
	}
	return project.Events(ours, now), nil
}

// metrics reads current usage. The metrics API is an aggregated APIService and is often absent or
// lagging, so a failure here is a degradation, never fatal.
func (c *Client) metrics(ctx context.Context, ns string, pods []corev1.Pod) (
	map[string]map[string]corev1.ResourceList, error) {

	want := map[string]bool{}
	for i := range pods {
		want[pods[i].Name] = true
	}
	l, err := c.Metrics.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]corev1.ResourceList{}
	for _, pm := range l.Items {
		if !want[pm.Name] {
			continue
		}
		byContainer := map[string]corev1.ResourceList{}
		for _, cm := range pm.Containers {
			byContainer[cm.Name] = cm.Usage
		}
		out[pm.Name] = byContainer
	}
	return out, nil
}

// services returns Services plausibly fronting this workload, with endpoint readiness.
func (c *Client) services(ctx context.Context, ns, workloadName string, sel map[string]string,
	pods []corev1.Pod) ([]model.ServiceView, error) {

	l, err := c.Typed.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var out []model.ServiceView
	for i := range l.Items {
		svc := &l.Items[i]
		if len(svc.Spec.Selector) == 0 {
			continue // headless or externalName; nothing to correlate
		}
		if !relatedSelector(svc.Spec.Selector, sel, svc.Name, workloadName) {
			continue
		}

		matched := 0
		s := labels.Set(svc.Spec.Selector).AsSelector()
		for j := range pods {
			if s.Matches(labels.Set(pods[j].Labels)) {
				matched++
			}
		}
		ready, notReady, err := c.endpointCounts(ctx, ns, svc.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, project.Service(svc, ready, notReady, matched))
	}
	return out, nil
}

// relatedSelector decides whether a Service is plausibly meant for this workload.
//
// The rule that matters is the semantic one: a Service fronts a workload when its selector is a
// SUBSET of the workload's pod labels, i.e. it would actually select those pods.
//
// Matching on any single shared key/value looks equivalent and is badly wrong in practice. Real
// clusters use release-scoped labels: every Argo CD component carries
// app.kubernetes.io/instance=argocd, so diagnosing argocd-server pulled in five sibling Services
// and reported all of them as broken. Verified against a live cluster, not reasoned about.
//
// The name fallback exists for the one case the subset rule cannot catch: a Service whose selector
// VALUE has a typo shares no complete pair with its workload, and that is exactly the failure the
// endpoint detector exists to find. Requiring the names to match keeps it from re-admitting the
// siblings.
//
// ponytail: a Service that is both misnamed and mislabelled is still missed. Catching it would mean
// pulling every Service in the namespace into every snapshot, which costs more context than the
// case is worth.
func relatedSelector(svcSel, workloadSel map[string]string, svcName, workloadName string) bool {
	if len(svcSel) == 0 {
		return false
	}
	subset := true
	for k, v := range svcSel {
		if wv, ok := workloadSel[k]; !ok || wv != v {
			subset = false
			break
		}
	}
	if subset {
		return true
	}
	if svcName == workloadName {
		for k := range svcSel {
			if _, ok := workloadSel[k]; ok {
				return true
			}
		}
	}
	return false
}

func (c *Client) endpointCounts(ctx context.Context, ns, svc string) (ready, notReady int, err error) {
	l, err := c.Typed.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + svc,
	})
	if err != nil {
		return 0, 0, err
	}
	for _, slice := range l.Items {
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				ready++
			} else {
				notReady++
			}
		}
	}
	return ready, notReady, nil
}

// nodes fetches only the nodes actually hosting these pods.
func (c *Client) nodes(ctx context.Context, pods []corev1.Pod, now time.Time) ([]model.NodeView, error) {
	seen := map[string]bool{}
	var names []string
	for i := range pods {
		if n := pods[i].Spec.NodeName; n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	out := make([]model.NodeView, 0, len(names))
	for _, n := range names {
		node, err := c.Typed.CoreV1().Nodes().Get(ctx, n, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		out = append(out, project.Node(node, now))
	}
	return out, nil
}

func (c *Client) hpa(ctx context.Context, ref Ref, now time.Time) (*model.HPAView, error) {
	l, err := c.Typed.AutoscalingV2().HorizontalPodAutoscalers(ref.Namespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range l.Items {
		h := &l.Items[i]
		if h.Spec.ScaleTargetRef.Kind != ref.Kind || h.Spec.ScaleTargetRef.Name != ref.Name {
			continue
		}
		v := &model.HPAView{
			Name: h.Name, MaxReplicas: h.Spec.MaxReplicas,
			Current: h.Status.CurrentReplicas, Desired: h.Status.DesiredReplicas,
		}
		if h.Spec.MinReplicas != nil {
			v.MinReplicas = *h.Spec.MinReplicas
		}
		for _, cond := range h.Status.Conditions {
			v.Conditions = append(v.Conditions, model.ConditionView{
				Type: string(cond.Type), Status: string(cond.Status),
				Reason: cond.Reason, Message: cond.Message,
				LastChangeSecsAgo: int64(now.Sub(cond.LastTransitionTime.Time).Seconds()),
			})
		}
		return v, nil
	}
	return nil, nil
}

func (c *Client) pdb(ctx context.Context, ns, workloadName string, sel map[string]string) (*model.PDBView, error) {
	l, err := c.Typed.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range l.Items {
		p := &l.Items[i]
		if p.Spec.Selector == nil {
			continue
		}
		if relatedSelector(p.Spec.Selector.MatchLabels, sel, p.Name, workloadName) {
			return project.PDB(p), nil
		}
	}
	return nil, nil
}

// unstructuredMap reads a nested map[string]string out of an unstructured object.
func unstructuredMap(obj map[string]any, path ...string) (map[string]string, bool, error) {
	cur := any(obj)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur, ok = m[p]
		if !ok {
			return nil, false, nil
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, false, nil
	}
	out := map[string]string{}
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, false, nil
		}
		out[k] = s
	}
	return out, true, nil
}
