package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/backendArchitect/argus/internal/detect"
	"github.com/backendArchitect/argus/internal/model"
)

// podListLimit bounds the namespace pod listing. Truncation is reported, never absorbed.
const podListLimit = 2000

// Trace follows the request path for one Service and reports where it gives out.
//
// Four list calls regardless of cluster size: Services (which also resolves the name),
// Ingresses, EndpointSlices, and the pods in the namespace.
//
// The pods are listed unfiltered rather than by the Service's own selector, which costs one
// list of one namespace and buys the near-miss analysis: a selector that matches nothing is
// only actionable once you know whether the pods are absent or merely mislabelled, and a
// selector-filtered list cannot tell those apart because it returns empty either way.
func (c *Client) Trace(ctx context.Context, service, namespace string) (*model.TraceReport, error) {
	ctx, cancel := c.WithTimeout(ctx)
	defer cancel()

	svc, err := c.resolveService(ctx, service, namespace)
	if err != nil {
		return nil, err
	}

	ch := model.ServiceChain{
		Service: svc.Name, Namespace: svc.Namespace,
		Type:                string(svc.Spec.Type),
		Selector:            svc.Spec.Selector,
		ExternalPolicyLocal: svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal,
	}
	for _, p := range svc.Spec.Ports {
		ch.Ports = append(ch.Ports, model.ServicePortSpec{
			Name: p.Name, Port: p.Port, Protocol: string(p.Protocol),
			TargetPort:   p.TargetPort.String(),
			TargetIsName: p.TargetPort.Type == intstr.String,
		})
	}

	ch.Routes, err = c.ingressRoutes(ctx, svc.Namespace, svc.Name)
	if err != nil {
		return nil, err
	}

	// Bounded so one enormous namespace cannot blow the context or the deadline. A truncated
	// list is recorded rather than absorbed: it would otherwise turn "the selector matches
	// nothing" into a confident wrong answer.
	pods, err := c.Typed.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{Limit: podListLimit})
	if err != nil {
		return nil, fmt.Errorf("listing pods in %s: %w", svc.Namespace, err)
	}
	ch.PodsTruncated = pods.Continue != ""
	ch.Matched, ch.NearMiss, ch.NearMissTotal = matchPods(svc.Spec.Selector, pods.Items)

	ready, notReady, err := c.endpointCounts(ctx, svc.Namespace, svc.Name)
	if err != nil {
		return nil, err
	}
	ch.EndpointsReady, ch.EndpointsNotReady = ready, notReady
	ch.EndpointPorts, err = c.endpointPorts(ctx, svc.Namespace, svc.Name)
	if err != nil {
		return nil, err
	}

	return detect.Trace(ch), nil
}

// resolveService finds a Service by fuzzy name, using the same tiering as workload resolution:
// exact, then prefix, then substring, stopping at the first tier that hits — so an exact name is
// never made ambiguous by an unrelated Service containing it. Ambiguity returns the candidates.
func (c *Client) resolveService(ctx context.Context, query, namespace string) (*corev1.Service, error) {
	if namespace == "" {
		return nil, fmt.Errorf("a namespace is required: a Service can only select pods beside it")
	}
	l, err := c.Typed.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services in %s: %w", namespace, err)
	}
	// Strip a kind prefix so `svc/checkout` works like the workload arguments do.
	query = strings.TrimPrefix(strings.TrimPrefix(query, "service/"), "svc/")

	for _, tier := range []func(string) bool{
		func(n string) bool { return n == query },
		func(n string) bool { return strings.HasPrefix(n, query) },
		func(n string) bool { return strings.Contains(n, query) },
	} {
		var hits []*corev1.Service
		for i := range l.Items {
			if tier(l.Items[i].Name) {
				hits = append(hits, &l.Items[i])
			}
		}
		switch len(hits) {
		case 0:
			continue
		case 1:
			return hits[0], nil
		default:
			cands := make([]Ref, 0, len(hits))
			for _, h := range hits {
				cands = append(cands, Ref{Kind: "Service", Name: h.Name, Namespace: h.Namespace})
			}
			sort.Slice(cands, func(i, j int) bool { return cands[i].Name < cands[j].Name })
			return nil, &AmbiguousError{Query: query, Candidates: cands, Noun: "services"}
		}
	}
	return nil, fmt.Errorf("no service matching %q in namespace %s", query, namespace)
}

// ingressRoutes finds every Ingress rule pointing at this Service.
func (c *Client) ingressRoutes(ctx context.Context, ns, svcName string) ([]model.IngressRoute, error) {
	l, err := c.Typed.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ingresses in %s: %w", ns, err)
	}

	var out []model.IngressRoute
	for i := range l.Items {
		ing := &l.Items[i]
		class := ""
		if ing.Spec.IngressClassName != nil {
			class = *ing.Spec.IngressClassName
		}
		// The default backend is a rule too, and a misdirected one produces the same 503.
		if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil && b.Service.Name == svcName {
			out = append(out, route(ing.Name, class, "", "(default backend)", b.Service.Port))
		}
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, path := range rule.HTTP.Paths {
				b := path.Backend.Service
				if b == nil || b.Name != svcName {
					continue
				}
				out = append(out, route(ing.Name, class, rule.Host, path.Path, b.Port))
			}
		}
	}
	return out, nil
}

func route(ingress, class, host, path string, p netv1.ServiceBackendPort) model.IngressRoute {
	r := model.IngressRoute{Ingress: ingress, Class: class, Host: host, Path: path}
	if p.Name != "" {
		r.BackendPort, r.BackendIsName = p.Name, true
	} else {
		r.BackendPort = fmt.Sprintf("%d", p.Number)
	}
	return r
}

// matchPods splits the namespace's pods into those the selector selects and those whose label
// value looks like a misspelling of what it wants.
//
// The near-miss half is what makes an empty match actionable. "Selector matches nothing" leaves
// the reader to work out whether the workload is absent, in another namespace, or mislabelled —
// and mislabelled is the common one.
//
// It deliberately does not match on a shared label KEY. That looks equivalent and over-matches
// badly: `app` is universal, so on a live cluster the key rule named six unrelated workloads and
// pointed the reader at bad-rollout when the answer was gapped. Same trap relatedSelector
// documents in gather.go, reached from the other direction.
func matchPods(sel map[string]string, pods []corev1.Pod) (matched []model.ChainPod, nearMiss []string, total int) {
	if len(sel) == 0 {
		return nil, nil, 0
	}
	s := labels.Set(sel).AsSelector()
	for i := range pods {
		p := &pods[i]
		// A terminal pod is not a backend; counting one as matched would report a Service as
		// correctly wired when the thing it selects has finished.
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if s.Matches(labels.Set(p.Labels)) {
			matched = append(matched, chainPod(p))
			continue
		}
		for k, want := range sel {
			if have, ok := p.Labels[k]; ok && plausibleTypo(have, want) {
				total++
				nearMiss = append(nearMiss, p.Name)
				break
			}
		}
	}
	// Bound the list: a mislabelled workload with forty replicas contributes forty names, and the
	// point is to show the reader the shape. The count above stays the real one.
	const maxNearMiss = 5
	sort.Strings(nearMiss)
	if len(nearMiss) > maxNearMiss {
		nearMiss = nearMiss[:maxNearMiss:maxNearMiss]
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Name < matched[j].Name })
	return matched, nearMiss, total
}

// plausibleTypo reports whether two label values are close enough that one is credibly a
// misspelling of the other, rather than two unrelated workloads sharing a label key.
//
// Substring in either direction catches the truncation and suffix cases (gapped vs gapped-api);
// a shared prefix catches transpositions and single-character slips, which are not substrings of
// each other (checkout-apo vs checkout-api share eleven characters and then diverge).
func plausibleTypo(have, want string) bool {
	if have == "" || want == "" || have == want {
		return false
	}
	if strings.Contains(have, want) || strings.Contains(want, have) {
		return true
	}
	const minShared = 4
	n := 0
	for n < len(have) && n < len(want) && have[n] == want[n] {
		n++
	}
	return n >= minShared
}

func chainPod(p *corev1.Pod) model.ChainPod {
	cp := model.ChainPod{Name: p.Name, Phase: string(p.Status.Phase)}
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			cp.Ready = cond.Status == corev1.ConditionTrue
		}
	}
	// Init containers are excluded deliberately: they have exited by the time traffic arrives, so
	// a port declared on one can never serve it.
	for i := range p.Spec.Containers {
		for _, port := range p.Spec.Containers[i].Ports {
			if port.Name != "" {
				cp.PortNames = append(cp.PortNames, port.Name)
			}
			cp.PortNumbers = append(cp.PortNumbers, port.ContainerPort)
		}
	}
	return cp
}

// endpointPorts reports the ports the EndpointSlices actually carry. Empty while pods matched is
// the dataplane's own confirmation that a targetPort resolved to nothing.
func (c *Client) endpointPorts(ctx context.Context, ns, svc string) ([]int32, error) {
	l, err := c.Typed.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.io/service-name=" + svc,
	})
	if err != nil {
		return nil, fmt.Errorf("listing endpointslices for %s: %w", svc, err)
	}
	seen := map[int32]bool{}
	var out []int32
	for i := range l.Items {
		for _, p := range l.Items[i].Ports {
			if p.Port != nil && !seen[*p.Port] {
				seen[*p.Port] = true
				out = append(out, *p.Port)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
