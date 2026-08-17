package detect

import (
	"fmt"
	"sort"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// detectEndpointGap finds Services routing to nothing.
//
// This is the classic silent failure: `kubectl get` shows a healthy Deployment
// and a Service with a valid ClusterIP, and traffic 503s forever. Nothing is in
// an error state, so nothing draws attention to it.
//
// The projection carries both MatchedPods and ReadyCount specifically so this
// detector can tell two very different incidents apart:
//
//	matched == 0            -> the selector matches no pods at all: a label typo.
//	matched > 0, ready == 0 -> the pods exist but none are ready: a readiness problem.
//
// They look identical in the API and have completely different fixes, so
// reporting "Service has no endpoints" without distinguishing them just moves
// the diagnosis one step down the road.
func detectEndpointGap(s *model.Snapshot) []model.Finding {
	// A workload scaled to zero has no endpoints by design, not by fault. Reporting its Services
	// as broken is a false positive on every intentionally-idle workload in the cluster — Argo CD
	// components parked at 0/0, scaled-down staging apps, cron-driven deployments.
	if s.Workload != nil && s.Workload.Desired == 0 {
		return nil
	}

	var out []model.Finding

	for i := range s.Services {
		svc := &s.Services[i]
		if svc.ReadyCount > 0 {
			continue
		}

		ev := []model.Evidence{
			evidence("service.spec", "svc/"+svc.Name, "selector is %s", labelString(svc.Selector)),
			evidence("endpointslice", "svc/"+svc.Name,
				"%d ready, %d not-ready endpoints; selector matches %d pods in the namespace",
				svc.ReadyCount, svc.NotReadyCount, svc.MatchedPods),
		}

		var id, title, detail string
		conf := 0.9
		if svc.MatchedPods == 0 {
			id = "endpoints.selector-matches-nothing"
			title = fmt.Sprintf("Service %q selects no pods — its selector does not match the workload", svc.Name)
			detail = fmt.Sprintf("The selector %s matches zero pods in this namespace, so the "+
				"Service has had no endpoints since it was created and every request to it "+
				"fails. The workload itself is healthy; the labels simply do not line up. "+
				"Compare it against the pod labels %s.",
				labelString(svc.Selector), labelString(podLabels(s)))
			if s.Workload != nil {
				ev = append(ev, evidence("workload.spec", workloadScope(s),
					"pod template labels are %s", labelString(s.Workload.Selector)))
			}
		} else {
			id = "endpoints.no-ready-backends"
			title = fmt.Sprintf("Service %q has no ready backends — its pods are not passing readiness", svc.Name)
			detail = fmt.Sprintf("The selector matches %d pod(s), so the labels are correct, but "+
				"none of them are ready. Traffic to this Service fails. The cause is the pods' "+
				"readiness, not the Service — look at why they are not passing their probe.",
				svc.MatchedPods)
			conf = 0.85
		}

		out = append(out, model.Finding{
			ID:         id,
			Severity:   model.Critical,
			Confidence: confidence(s, conf, "services"),
			Scope:      workloadScope(s),
			Title:      title,
			Detail:     detail,
			Evidence:   ev,
		})
	}
	return out
}

// labelString renders a label map as sorted key=value pairs.
//
// Go's %v prints a map as "map[app:gapped-api]", which is Go syntax leaking into
// text a human and a model both have to read, and map iteration order makes it
// non-deterministic on top of that.
func labelString(m map[string]string) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

// podLabels returns the labels of the first gathered pod, for comparison against
// a Service selector that matches nothing.
func podLabels(s *model.Snapshot) map[string]string {
	if len(s.Pods) == 0 {
		return nil
	}
	return s.Pods[0].Labels
}
