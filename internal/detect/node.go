package detect

import (
	"fmt"
	"sort"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// pressureConditions are node conditions that explain workload failures the
// workload did not cause.
var pressureConditions = map[string]string{
	"MemoryPressure":     "is low on memory and will evict pods",
	"DiskPressure":       "is low on disk and will evict pods",
	"PIDPressure":        "has too few process IDs available to start containers",
	"NetworkUnavailable": "has no working network configuration",
}

// detectNodePressure widens scope from the workload to its host.
//
// If the node is unhealthy, the workload's symptoms are downstream of that, and
// reporting them as workload problems sends someone to debug an application that
// is working fine.
//
// Critically, this must NOT fire merely because several failing pods share a
// node. On a single-node cluster — including every kind cluster and every
// fixture in testdata/broken — ALL pods share a node, so co-location carries no
// information whatsoever. The evidence required is an actual abnormal node
// condition. The healthy control fixture exists in large part to prove this
// detector stays quiet.
func detectNodePressure(s *model.Snapshot) []model.Finding {
	var out []model.Finding

	for i := range s.Nodes {
		node := &s.Nodes[i]

		var problems []string
		var ev []model.Evidence
		for _, c := range node.Conditions {
			if _, ok := pressureConditions[c.Type]; ok && c.Status == "True" {
				problems = append(problems, fmt.Sprintf("%s (%s)", c.Type, c.Reason))
				ev = append(ev, evidence("node.condition", "node/"+node.Name,
					"%s=True since %ds ago: %s", c.Type, c.LastChangeSecsAgo, c.Message))
			}
		}
		if !node.Ready {
			problems = append(problems, "NotReady")
			ev = append(ev, evidence("node.condition", "node/"+node.Name,
				"node is not Ready; its pods will be evicted once the toleration expires"))
		}
		if len(problems) == 0 {
			continue
		}

		affected := podsOnNode(s, node.Name)
		ev = append(ev, evidence("pod.status", "node/"+node.Name,
			"%d of this workload's pods are scheduled here: %s",
			len(affected), strings.Join(affected, ", ")))

		sort.Strings(problems)
		out = append(out, model.Finding{
			ID:         "node.unhealthy-host",
			Severity:   model.Critical,
			Confidence: confidence(s, 0.85, "nodes"),
			Scope:      "node/" + node.Name,
			Title: fmt.Sprintf("Node %s is unhealthy (%s) — the workload is a symptom, not the cause",
				node.Name, strings.Join(problems, ", ")),
			Detail: fmt.Sprintf("Node %s reports %s. The kubelet %s. %d pod(s) of this workload "+
				"are scheduled here, so their failures are downstream of the node's condition. "+
				"Debugging the application will not help until the node recovers or the pods "+
				"are rescheduled elsewhere.",
				node.Name, strings.Join(problems, " and "),
				conditionEffect(node), len(affected)),
			Evidence: ev,
			// Scope widening: the node explains the pod-level symptoms, so suppress
			// them rather than reporting one cause three times.
			Suppresses: []string{"oomkill.limit-too-low", "probe.readiness-misconfigured"},
		})
	}
	return out
}

func conditionEffect(n *model.NodeView) string {
	for _, c := range n.Conditions {
		if desc, ok := pressureConditions[c.Type]; ok && c.Status == "True" {
			return desc
		}
	}
	return "cannot schedule or run pods normally here"
}

func podsOnNode(s *model.Snapshot, node string) []string {
	var out []string
	for i := range s.Pods {
		if s.Pods[i].Node == node {
			out = append(out, s.Pods[i].Name)
		}
	}
	sort.Strings(out)
	return out
}
