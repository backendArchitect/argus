package detect

import (
	"fmt"
	"sort"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// Hop statuses.
const (
	hopOK      = "ok"
	hopBroken  = "broken"
	hopWarn    = "warn"
	hopSkipped = "skipped"
)

// Trace walks the request path for one Service and reports the first hop that gives out.
//
// Pure, like every detector: it takes a data-only chain and returns hops, so the interesting
// shapes are pinned by table tests rather than by standing up an Ingress controller.
//
// Only the FIRST failing hop is marked broken. Everything downstream of a break is marked
// skipped, because a selector that matches nothing guarantees no endpoints and nothing ready —
// reporting those as three faults describes one fault three times, which is how a diagnosis
// stops being one.
func Trace(ch model.ServiceChain) *model.TraceReport {
	r := &model.TraceReport{
		Service: ch.Service, Namespace: ch.Namespace,
		NotChecked: TraceNotChecked(ch),
	}
	add := func(step, status, detail string, remedy ...string) {
		// Past the first break every hop is a consequence, not a finding — including one the
		// check itself marked skipped. Leaving such a hop its local explanation ("no addresses
		// to be ready") states a fact that reads as a cause, when the cause is upstream.
		if r.BrokenAt != "" {
			status, detail = hopSkipped, fmt.Sprintf(
				"not reachable — the chain already breaks at %q", r.BrokenAt)
			remedy = nil
		}
		h := model.Hop{Step: step, Status: status, Detail: detail}
		if len(remedy) > 0 {
			h.Remedy = remedy[0]
		}
		r.Hops = append(r.Hops, h)
		if status == hopBroken && r.BrokenAt == "" {
			r.BrokenAt = step
		}
	}

	traceIngress(ch, add)
	traceSelector(ch, add)
	tracePorts(ch, add)
	traceEndpoints(ch, add)
	return r
}

// traceIngress checks that each Ingress rule naming this Service names a port it actually has.
//
// A backend port that does not resolve is a hard failure: the controller has no endpoint to
// route to and answers 503 while `kubectl get ingress,svc,pods` shows nothing wrong.
func traceIngress(ch model.ServiceChain, add func(string, string, string, ...string)) {
	if len(ch.Routes) == 0 {
		how := "in-cluster callers reach it by DNS"
		if ch.Type == "LoadBalancer" || ch.Type == "NodePort" {
			how = "it is exposed directly as type " + ch.Type
		}
		add("ingress", hopSkipped, fmt.Sprintf(
			"No Ingress in %s routes to this Service, so %s. If external traffic was supposed to "+
				"arrive through an Ingress, its absence is the answer.", ch.Namespace, how))
		return
	}

	var bad []string
	badByName := false
	for _, rt := range ch.Routes {
		if servicePortExists(ch.Ports, rt.BackendPort, rt.BackendIsName) {
			continue
		}
		// Whether the FAILING route names its port or numbers it, not whichever route happened
		// to be first: a manifest can mix the two, and the remedy would name the wrong one.
		badByName = badByName || rt.BackendIsName
		bad = append(bad, fmt.Sprintf("%s (%s%s) → port %s",
			rt.Ingress, rt.Host, rt.Path, rt.BackendPort))
	}
	if len(bad) == 0 {
		add("ingress", hopOK, fmt.Sprintf("%d Ingress rule(s) route here, each naming a port "+
			"this Service declares: %s", len(ch.Routes), routeSummary(ch.Routes)))
		return
	}
	kind := "number"
	if badByName {
		kind = "name"
	}
	add("ingress", hopBroken, fmt.Sprintf(
		"%d Ingress rule(s) route to a port this Service does not declare: %s. The Service "+
			"declares %s. The controller cannot resolve a backend and will answer 503 with every "+
			"object below it healthy.", len(bad), strings.Join(bad, "; "), portList(ch.Ports)),
		fmt.Sprintf("point the rule's backend port %s at one the Service declares, or add the "+
			"missing port to the Service", kind))
}

// traceSelector checks the Service actually selects pods.
func traceSelector(ch model.ServiceChain, add func(string, string, string, ...string)) {
	if len(ch.Selector) == 0 {
		// A selector-less Service is not broken: its Endpoints are managed by hand, or it is
		// an ExternalName. Both are legitimate and neither is the selector-driven path argus
		// traces, so say which one this is rather than reporting a fault.
		add("selector", hopSkipped, "This Service has no selector, so its endpoints are managed "+
			"manually (or it is an ExternalName). argus traces the selector-driven path; the "+
			"endpoint checks below still apply.")
		return
	}
	if len(ch.Matched) > 0 {
		ready := 0
		for _, p := range ch.Matched {
			if p.Ready {
				ready++
			}
		}
		add("selector", hopOK, fmt.Sprintf("%s matches %d pod(s), %d ready",
			labelPairs(ch.Selector), len(ch.Matched), ready))
		return
	}
	truncated := ""
	if ch.PodsTruncated {
		truncated = " NOTE: the namespace holds more pods than argus listed, so this is not " +
			"conclusive — a matching pod may exist beyond the limit."
	}
	if len(ch.NearMiss) > 0 {
		add("selector", hopBroken, fmt.Sprintf(
			"%s matches no pods, but %d pod(s) carry a label value that looks like a misspelling "+
				"of it: %s. That is a label typo, not a missing workload.%s",
			labelPairs(ch.Selector), ch.NearMissTotal, strings.Join(ch.NearMiss, ", "), truncated),
			"compare the Service selector against the pod template's labels — one of the two has "+
				"the wrong value")
		return
	}
	add("selector", hopBroken, fmt.Sprintf(
		"%s matches no pods in %s, and no pod there carries any of those label keys at all.%s",
		labelPairs(ch.Selector), ch.Namespace, truncated),
		"either the workload is not deployed in this namespace, or the Service is in the wrong "+
			"one — a Service can only select pods beside it")
}

// tracePorts resolves each Service port against the ports the matched pods declare.
//
// The asymmetry here is the point, and it is not cosmetic. A NAMED targetPort that matches no
// containerPort name cannot be resolved by the endpoints controller, so the Service is programmed
// with no port and every connection is refused — that is a proven break. A NUMERIC targetPort
// works whether or not any container declares it, because containerPort is informational and a
// process may listen on a port it never declared. Reporting the numeric case as broken would be
// a false positive on a legitimate, if untidy, manifest.
func tracePorts(ch model.ServiceChain, add func(string, string, string, ...string)) {
	if len(ch.Ports) == 0 {
		add("target-port", hopBroken, "This Service declares no ports, so there is nothing for "+
			"traffic to arrive on.", "add a ports entry naming the container's port")
		return
	}
	if len(ch.Matched) == 0 {
		add("target-port", hopSkipped, "No pods matched, so there are no container ports to "+
			"resolve against.")
		return
	}

	names, numbers := declaredPorts(ch.Matched)
	var broken, warn, okPorts []string
	for _, p := range ch.Ports {
		switch {
		case p.TargetIsName && !contains(names, p.TargetPort):
			broken = append(broken, fmt.Sprintf(
				"port %d targets a containerPort NAMED %q, which no container in the %d matched "+
					"pod(s) declares (declared names: %s)",
				p.Port, p.TargetPort, len(ch.Matched), noneIfEmpty(strings.Join(names, ", "))))
		case !p.TargetIsName && !containsNum(numbers, p.TargetPort):
			warn = append(warn, fmt.Sprintf(
				"port %d targets %s, which no container declares (declared: %s)",
				p.Port, p.TargetPort, noneIfEmpty(numberList(numbers))))
		default:
			okPorts = append(okPorts, fmt.Sprintf("%d→%s", p.Port, p.TargetPort))
		}
	}

	switch {
	case len(broken) > 0:
		// The dataplane confirms it independently, which is worth stating: it separates
		// "argus reasoned about your manifest" from "the cluster agrees".
		confirm := ""
		if len(ch.EndpointPorts) == 0 && ch.EndpointsReady+ch.EndpointsNotReady > 0 {
			confirm = " The EndpointSlices carry no port at all, which is the cluster confirming it."
		}
		add("target-port", hopBroken, fmt.Sprintf(
			"%s. A named targetPort that resolves to nothing leaves the Service with no port to "+
				"forward to, so connections are refused while the Service, the pods and the "+
				"endpoints all report healthy.%s", strings.Join(broken, "; "), confirm),
			"either add that name to the containerPort in the pod template, or change targetPort "+
				"to the number the container listens on")
	case len(warn) > 0:
		add("target-port", hopWarn, fmt.Sprintf(
			"%s. containerPort is declarative and Kubernetes never verifies it, so this is a "+
				"strong hint rather than proof — traffic still arrives if the process is in fact "+
				"listening there.", strings.Join(warn, "; ")))
	default:
		add("target-port", hopOK, fmt.Sprintf("every Service port resolves to a declared "+
			"containerPort: %s", strings.Join(okPorts, ", ")))
	}
}

// traceEndpoints checks the two things kube-proxy cares about: that addresses exist, and that
// they are ready. Only ready addresses are programmed, so the distinction decides whether
// traffic has anywhere to go.
func traceEndpoints(ch model.ServiceChain, add func(string, string, string, ...string)) {
	total := ch.EndpointsReady + ch.EndpointsNotReady
	if total == 0 {
		detail := "The Service has no endpoint addresses."
		remedy := "check whether the pods have been assigned IPs and reached Running"
		if len(ch.Matched) > 0 {
			detail += fmt.Sprintf(" %d pod(s) match its selector, so they exist but have not "+
				"produced an address — pods that are Pending or have no IP yet never do.",
				len(ch.Matched))
		}
		add("endpoints", hopBroken, detail, remedy)
		add("readiness", hopSkipped, "no addresses to be ready")
		return
	}
	add("endpoints", hopOK, fmt.Sprintf("%d endpoint address(es) exist", total))

	if ch.EndpointsReady == 0 {
		add("readiness", hopBroken, fmt.Sprintf(
			"All %d endpoint address(es) are NOT ready. kube-proxy programs only ready "+
				"addresses, so the Service resolves but has nothing behind it — connections are "+
				"refused or time out.", ch.EndpointsNotReady),
			"this is a readiness probe or a slow start, not a networking fault: read the "+
				"container's logs and compare its startup time against initialDelaySeconds")
		return
	}
	msg := fmt.Sprintf("%d address(es) ready", ch.EndpointsReady)
	if ch.EndpointsNotReady > 0 {
		add("readiness", hopWarn, fmt.Sprintf("%s, %d not — traffic is served, at reduced "+
			"capacity", msg, ch.EndpointsNotReady))
		return
	}
	add("readiness", hopOK, msg+", so traffic reaching the Service is forwarded")
}

// TraceNotChecked states what this trace does not cover.
//
// Longer than the other tools' equivalents, and deliberately so: argus checks the DECLARED
// chain, and several of the most common real causes of "the Service is up and traffic still
// fails" live entirely outside it. A reader who takes an intact chain as a clean bill of health
// has been misled by the tool, so the intact case has to point somewhere.
func TraceNotChecked(ch model.ServiceChain) []string {
	out := []string{
		"whether the process is actually listening on the target port — containerPort is " +
			"declarative metadata and Kubernetes never verifies it",
		"NetworkPolicy, which can drop this traffic with every hop above reported healthy",
		"service mesh sidecars — Istio or Linkerd mTLS and authorization policy reject " +
			"connections after the endpoint has been programmed",
		"the ingress controller's own health, and whether it watches this Ingress's class",
		"DNS resolution of the Service name from wherever the caller runs",
		"kube-proxy and the CNI dataplane on each node",
	}
	if ch.ExternalPolicyLocal {
		// Worth its own line and worth being first: the Service demonstrably sets it, and it
		// drops traffic while every hop above stays green.
		out = append([]string{
			"this Service sets externalTrafficPolicy: Local, which discards external traffic " +
				"arriving on any node with no local ready pod — argus does not check per-node " +
				"pod placement",
		}, out...)
	}
	return out
}

func servicePortExists(ports []model.ServicePortSpec, want string, byName bool) bool {
	for _, p := range ports {
		if byName && p.Name == want {
			return true
		}
		if !byName && fmt.Sprintf("%d", p.Port) == want {
			return true
		}
	}
	return false
}

// declaredPorts collects the port names and numbers declared across the matched pods.
//
// Deduplicated and sorted: the same template usually produces identical ports on every pod, and
// an unsorted list would make the message flap between runs the way map iteration does.
func declaredPorts(pods []model.ChainPod) (names []string, numbers []int32) {
	seenN, seenP := map[string]bool{}, map[int32]bool{}
	for _, p := range pods {
		for _, n := range p.PortNames {
			if n != "" && !seenN[n] {
				seenN[n] = true
				names = append(names, n)
			}
		}
		for _, n := range p.PortNumbers {
			if !seenP[n] {
				seenP[n] = true
				numbers = append(numbers, n)
			}
		}
	}
	sort.Strings(names)
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	return names, numbers
}

func contains(hay []string, want string) bool {
	for _, h := range hay {
		if h == want {
			return true
		}
	}
	return false
}

func containsNum(hay []int32, want string) bool {
	for _, h := range hay {
		if fmt.Sprintf("%d", h) == want {
			return true
		}
	}
	return false
}

func numberList(ns []int32) string {
	parts := make([]string, 0, len(ns))
	for _, n := range ns {
		parts = append(parts, fmt.Sprintf("%d", n))
	}
	return strings.Join(parts, ", ")
}

func portList(ports []model.ServicePortSpec) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		s := fmt.Sprintf("%d", p.Port)
		if p.Name != "" {
			s += fmt.Sprintf(" (name %q)", p.Name)
		}
		parts = append(parts, s)
	}
	return noneIfEmpty(strings.Join(parts, ", "))
}

func routeSummary(routes []model.IngressRoute) string {
	parts := make([]string, 0, len(routes))
	for _, rt := range routes {
		parts = append(parts, fmt.Sprintf("%s%s → %s", rt.Host, rt.Path, rt.BackendPort))
	}
	return strings.Join(parts, "; ")
}

func labelPairs(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for _, kv := range sortedPairs(m) {
		parts = append(parts, kv[0]+"="+kv[1])
	}
	return strings.Join(parts, ",")
}

func countStatus(hops []model.Hop, status string) int {
	n := 0
	for _, h := range hops {
		if h.Status == status {
			n++
		}
	}
	return n
}

func noneIfEmpty(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// RenderTrace formats the chain in traffic order, so the reader's eye stops at the first ✗.
func RenderTrace(r *model.TraceReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SERVICE PATH   %s/%s\n", r.Namespace, r.Service)
	switch warn := countStatus(r.Hops, hopWarn); {
	case r.BrokenAt != "":
		fmt.Fprintf(&b, "verdict        breaks at %q\n", r.BrokenAt)
	case warn > 0:
		// "Intact" printed above a ! is a small dishonesty that costs the reader a second look.
		fmt.Fprintf(&b, "verdict        no break found, %d hop(s) worth checking\n", warn)
	default:
		fmt.Fprintf(&b, "verdict        the declared chain is intact\n")
	}

	b.WriteString("\n")
	for _, h := range r.Hops {
		mark := map[string]string{hopOK: "✓", hopBroken: "✗", hopWarn: "!", hopSkipped: "·"}[h.Status]
		fmt.Fprintf(&b, "  %s %-13s %s\n", mark, h.Step, model.Wrap(h.Detail, 18))
		if h.Remedy != "" {
			fmt.Fprintf(&b, "    %-13s fix: %s\n", "", model.Wrap(h.Remedy, 23))
		}
	}

	b.WriteString("\n")
	if r.BrokenAt == "" {
		// The intact case is the one where a bare list of gaps is actually the answer, so it
		// gets a heading that says as much rather than the usual caveat framing.
		b.WriteString("Every hop argus can see is healthy, so the cause is in something it cannot:\n")
	} else {
		b.WriteString("not checked by argus:\n")
	}
	for _, n := range r.NotChecked {
		fmt.Fprintf(&b, "  · %s\n", model.Wrap(n, 4))
	}
	return b.String()
}
