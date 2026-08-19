package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Ref identifies a workload controller.
type Ref struct {
	Kind      string // Deployment | StatefulSet | DaemonSet | Rollout
	Name      string
	Namespace string
}

func (r Ref) String() string { return strings.ToLower(r.Kind) + "/" + r.Namespace + "/" + r.Name }

// kindAliases maps the prefixes people actually type to canonical kinds.
var kindAliases = map[string]string{
	"deploy": "Deployment", "deployment": "Deployment", "deployments": "Deployment",
	"sts": "StatefulSet", "statefulset": "StatefulSet", "statefulsets": "StatefulSet",
	"ds": "DaemonSet", "daemonset": "DaemonSet", "daemonsets": "DaemonSet",
	"ro": "Rollout", "rollout": "Rollout", "rollouts": "Rollout",
}

var rolloutGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}

// AmbiguousError lists the candidates when a fuzzy name matches more than one workload. Returning
// the choices beats picking one: during an incident, silently diagnosing the wrong workload costs
// more than one extra round-trip.
type AmbiguousError struct {
	Query      string
	Candidates []Ref
	// Noun names what was being resolved. Empty means workloads, which is the common case and
	// the wording USAGE documents; trace_service_path resolves Services through the same tiers
	// and would otherwise report them as workloads.
	Noun string
}

func (e *AmbiguousError) Error() string {
	names := make([]string, len(e.Candidates))
	for i, c := range e.Candidates {
		names[i] = c.String()
	}
	noun := "workloads"
	if e.Noun != "" {
		noun = e.Noun
	}
	return fmt.Sprintf("%q matches %d %s: %s", e.Query, len(e.Candidates), noun, strings.Join(names, ", "))
}

// Resolve turns a fuzzy query into exactly one Ref.
//
// Accepts "checkout", "checkout-api", "deploy/checkout-api", or "deployment/checkout-api".
// Matching is tiered — exact name first, then prefix, then substring — and stops at the first tier
// that produces hits, so an exact name is never made ambiguous by an unrelated substring match.
func (c *Client) Resolve(ctx context.Context, query, namespace string) (Ref, error) {
	wantKind := ""
	if k, rest, ok := strings.Cut(query, "/"); ok {
		if canonical, known := kindAliases[strings.ToLower(k)]; known {
			wantKind, query = canonical, rest
		} else {
			return Ref{}, fmt.Errorf("unknown kind %q in %q (want deploy, sts, ds or rollout)", k, query)
		}
	}
	if namespace == "" {
		return Ref{}, fmt.Errorf("namespace is required to resolve %q", query)
	}

	all, err := c.listWorkloads(ctx, namespace, wantKind)
	if err != nil {
		return Ref{}, err
	}
	if len(all) == 0 {
		return Ref{}, fmt.Errorf("no workloads in namespace %q", namespace)
	}

	q := strings.ToLower(query)
	tiers := []func(string) bool{
		func(n string) bool { return n == q },
		func(n string) bool { return strings.HasPrefix(n, q) },
		func(n string) bool { return strings.Contains(n, q) },
	}
	for _, match := range tiers {
		var hits []Ref
		for _, r := range all {
			if match(strings.ToLower(r.Name)) {
				hits = append(hits, r)
			}
		}
		switch {
		case len(hits) == 1:
			return hits[0], nil
		case len(hits) > 1:
			sort.Slice(hits, func(i, j int) bool { return hits[i].Name < hits[j].Name })
			return Ref{}, &AmbiguousError{Query: query, Candidates: hits}
		}
	}
	return Ref{}, fmt.Errorf("no workload matching %q in namespace %q", query, namespace)
}

// listWorkloads enumerates candidate controllers. Rollouts are best-effort: the CRD is absent on
// most clusters and its absence is not an error.
func (c *Client) listWorkloads(ctx context.Context, ns, wantKind string) ([]Ref, error) {
	var out []Ref
	add := func(kind, name string) {
		if wantKind == "" || wantKind == kind {
			out = append(out, Ref{Kind: kind, Name: name, Namespace: ns})
		}
	}

	if wantKind == "" || wantKind == "Deployment" {
		l, err := c.Typed.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing deployments in %s: %w", ns, err)
		}
		for _, d := range l.Items {
			add("Deployment", d.Name)
		}
	}
	if wantKind == "" || wantKind == "StatefulSet" {
		l, err := c.Typed.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing statefulsets in %s: %w", ns, err)
		}
		for _, s := range l.Items {
			add("StatefulSet", s.Name)
		}
	}
	if wantKind == "" || wantKind == "DaemonSet" {
		l, err := c.Typed.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing daemonsets in %s: %w", ns, err)
		}
		for _, d := range l.Items {
			add("DaemonSet", d.Name)
		}
	}
	if wantKind == "" || wantKind == "Rollout" {
		if l, err := c.Dynamic.Resource(rolloutGVR).Namespace(ns).List(ctx, metav1.ListOptions{}); err == nil {
			for _, r := range l.Items {
				add("Rollout", r.GetName())
			}
		} // ponytail: Argo not installed is the common case, not a failure worth reporting
	}
	return out, nil
}
