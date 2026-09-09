package detect

import (
	"fmt"

	"github.com/backendArchitect/argus/internal/model"
)

// This file exists because the gather layer was already paying for HPA and PDB on every diagnosis
// and no detector read either one. That is the worst of both: the apiserver calls and the token
// budget were spent, and nothing was concluded from them.
//
// Both detectors here are warnings rather than criticals, and that is deliberate. Neither
// condition stops a workload serving traffic right now, so neither should ever displace the
// critical that explains an actual outage — severity dominates the ranking, so a 0.9-confidence
// warning stays below a 0.5-confidence critical.

// detectHPACannotScale finds an autoscaler that is not autoscaling.
//
// The failure this catches is silent by construction. `kubectl get hpa` shows the object, the
// Deployment is healthy, and the replica count looks deliberate — but the HPA has given up
// computing a target, so the workload is pinned wherever it happens to sit and will not respond to
// load at all. The next traffic spike is then an outage with no proximate cause in any log.
//
// Keyed on the HPA's own ScalingActive condition rather than on inferred numbers: the controller
// already publishes a verdict on whether it can act, and second-guessing it from replica counts
// would guess wrong on a workload that is legitimately steady.
func detectHPACannotScale(s *model.Snapshot) []model.Finding {
	h := s.HPA
	if h == nil {
		return nil
	}
	cond := condition(h.Conditions, "ScalingActive")
	if cond == nil || cond.Status != "False" {
		return nil
	}

	// AbleToScale=False is a different fault with a different owner — the controller cannot reach
	// the scale subresource at all, rather than being unable to decide a number. Say which it is
	// instead of blurring them, since the first is usually RBAC or a missing target.
	detail := fmt.Sprintf("The HorizontalPodAutoscaler %q is not scaling: the controller reports "+
		"ScalingActive=False (%s), so it cannot compute a replica count and the workload is "+
		"effectively pinned at %d of a possible %d. Nothing about this is visible in the "+
		"Deployment, which will look healthy while being unable to respond to load. The usual "+
		"cause is the metrics API being absent or unreachable — an HPA targeting CPU or memory "+
		"needs metrics-server installed, and a custom metric needs its adapter healthy.",
		h.Name, cond.Reason, h.Current, h.MaxReplicas)
	if able := condition(h.Conditions, "AbleToScale"); able != nil && able.Status == "False" {
		detail += fmt.Sprintf(" Note AbleToScale is also False (%s): the controller cannot reach "+
			"the target's scale subresource either, which points at the scaleTargetRef or RBAC "+
			"rather than at metrics.", able.Reason)
	}

	return []model.Finding{{
		ID:       "hpa.cannot-scale",
		Severity: model.Warning,
		// The controller is quoting itself, so this is not inferred. It is docked only for the
		// metrics gap, which is the thing that most often causes it and which the snapshot can
		// independently confirm or fail to confirm.
		Confidence: confidence(s, 0.9, "metrics"),
		Scope:      workloadScope(s),
		Title:      "The autoscaler cannot scale this workload",
		Detail:     detail,
		Evidence: []model.Evidence{
			evidence("hpa.status", "hpa/"+h.Name,
				"ScalingActive=False, reason=%s: %s", cond.Reason, firstLine(cond.Message)),
			evidence("hpa.spec", "hpa/"+h.Name,
				"min %d, max %d replicas; currently %d, and the controller wants %d",
				h.MinReplicas, h.MaxReplicas, h.Current, h.Desired),
		},
	}}
}

// detectPDBBlocksDrain finds a PodDisruptionBudget that permits no voluntary disruption while the
// workload it guards is perfectly healthy.
//
// This is the cause of the incident that does not look like one: a node drain, cluster upgrade or
// autoscaler scale-down that hangs for hours with no error anywhere near the workload. `kubectl
// drain` reports only that it cannot evict a pod, and the reason lives on an object nobody thinks
// to check because nothing is wrong with it.
//
// The health gate is the whole discriminator, and skipping it would make this detector actively
// harmful. Any broken workload also has disruptionsAllowed 0 — that is the PDB working exactly as
// intended, protecting what few healthy replicas remain — so firing on it would add a confident
// distraction to every real outage. Reported only when currentHealthy has met the bar and the
// budget STILL leaves no headroom, which means the constraint is structural rather than a symptom:
// most often minAvailable equal to the replica count, or a one-replica workload with any
// minAvailable at all.
func detectPDBBlocksDrain(s *model.Snapshot) []model.Finding {
	p := s.PDB
	if p == nil || p.DisruptionsAllowed > 0 {
		return nil
	}
	// Unhealthy: the budget is a symptom of the outage, not a finding about the budget.
	if p.CurrentHealthy < p.DesiredHealthy {
		return nil
	}
	// A workload scaled to zero has nothing to disrupt and no drain to block.
	if p.CurrentHealthy == 0 {
		return nil
	}

	return []model.Finding{{
		ID:       "pdb.blocks-disruption",
		Severity: model.Warning,
		// Arithmetic on three fields the PDB controller published, with nothing inferred.
		Confidence: 0.9,
		Scope:      workloadScope(s),
		Title:      "The disruption budget permits no voluntary eviction",
		Detail: fmt.Sprintf("PodDisruptionBudget %q allows 0 disruptions even though the workload "+
			"is healthy (%d pods healthy against %d required). The workload is serving normally, "+
			"so this is not why it is broken — but no pod of it can be evicted voluntarily, which "+
			"means a node drain, a cluster upgrade or an autoscaler scale-down touching one of "+
			"these pods will block indefinitely rather than fail. The budget is at its floor "+
			"structurally, not because of a transient problem: either minAvailable equals the "+
			"replica count, or this is a single-replica workload where any minAvailable forbids "+
			"every eviction. Raising the replica count above minAvailable restores headroom.",
			p.Name, p.CurrentHealthy, p.DesiredHealthy),
		Evidence: []model.Evidence{
			evidence("pdb.status", "pdb/"+p.Name,
				"disruptionsAllowed=0 with currentHealthy=%d and desiredHealthy=%d",
				p.CurrentHealthy, p.DesiredHealthy),
		},
	}}
}

// condition finds a condition by type, or nil. Shared by both detectors here because HPA and
// workload conditions are the same shape.
func condition(conds []model.ConditionView, typ string) *model.ConditionView {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}
