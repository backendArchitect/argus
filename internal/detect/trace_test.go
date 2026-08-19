package detect

import (
	"strings"
	"testing"

	"github.com/backendArchitect/argus/internal/model"
)

// healthyChain is the shape everything else deviates from: an Ingress naming a port the Service
// declares, a selector that matches, a named targetPort the container declares, ready endpoints.
func healthyChain() model.ServiceChain {
	return model.ServiceChain{
		Service: "checkout-api", Namespace: "prod", Type: "ClusterIP",
		Selector: map[string]string{"app": "checkout-api"},
		Ports: []model.ServicePortSpec{
			{Name: "http", Port: 80, Protocol: "TCP", TargetPort: "http", TargetIsName: true},
		},
		Routes: []model.IngressRoute{
			{Ingress: "web", Host: "shop.example.com", Path: "/api", BackendPort: "http", BackendIsName: true},
		},
		Matched: []model.ChainPod{
			{Name: "checkout-api-1", Ready: true, Phase: "Running",
				PortNames: []string{"http"}, PortNumbers: []int32{8080}},
		},
		EndpointsReady: 1, EndpointPorts: []int32{8080},
	}
}

// TestTraceFindsTheFirstBreak is the core assertion: the reported break is the right hop, and
// exactly one hop is reported broken. A trace that flags three hops for one fault is describing
// a symptom chain, not a cause.
func TestTraceFindsTheFirstBreak(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*model.ServiceChain)
		wantHop  string // "" means the chain must be intact
		wantWord string // a phrase the broken hop's detail must contain
	}{
		{
			name:    "healthy chain reports no break",
			mutate:  func(*model.ServiceChain) {},
			wantHop: "",
		},
		{
			// The headline case. Nothing in `kubectl get svc,pods,ingress` looks wrong.
			name: "named targetPort matching no containerPort",
			mutate: func(c *model.ServiceChain) {
				c.Matched[0].PortNames = []string{"web"}
			},
			wantHop:  "target-port",
			wantWord: "no container in the 1 matched pod(s) declares",
		},
		{
			// Must NOT be broken: containerPort is informational, so a numeric targetPort works
			// whether or not any container declares it. Reporting this as the cause would be a
			// false positive on a legitimate manifest.
			name: "numeric targetPort matching no containerPort is a warning, not a break",
			mutate: func(c *model.ServiceChain) {
				c.Ports[0].TargetPort, c.Ports[0].TargetIsName = "9999", false
			},
			wantHop: "",
		},
		{
			name: "ingress naming a port the Service does not declare",
			mutate: func(c *model.ServiceChain) {
				c.Routes[0].BackendPort = "grpc"
			},
			wantHop:  "ingress",
			wantWord: "does not declare",
		},
		{
			name: "selector typo, with pods carrying the same key",
			mutate: func(c *model.ServiceChain) {
				c.Selector = map[string]string{"app": "checkout-apo"}
				c.Matched = nil
				c.NearMiss = []string{"checkout-api-1"}
				c.EndpointsReady = 0
				c.EndpointPorts = nil
			},
			wantHop:  "selector",
			wantWord: "label typo",
		},
		{
			name: "selector matches nothing and no pod carries the key",
			mutate: func(c *model.ServiceChain) {
				c.Selector = map[string]string{"app": "gone"}
				c.Matched = nil
				c.EndpointsReady = 0
				c.EndpointPorts = nil
			},
			wantHop:  "selector",
			wantWord: "carries any of those label keys at all",
		},
		{
			name: "pods matched but no endpoint address exists",
			mutate: func(c *model.ServiceChain) {
				c.Matched[0].Ready = false
				c.Matched[0].Phase = "Pending"
				c.EndpointsReady = 0
				c.EndpointPorts = nil
			},
			wantHop:  "endpoints",
			wantWord: "no endpoint addresses",
		},
		{
			name: "addresses exist but none are ready",
			mutate: func(c *model.ServiceChain) {
				c.Matched[0].Ready = false
				c.EndpointsReady, c.EndpointsNotReady = 0, 3
			},
			wantHop:  "readiness",
			wantWord: "kube-proxy programs only ready",
		},
		{
			name: "Service declares no ports at all",
			mutate: func(c *model.ServiceChain) {
				c.Ports = nil
				c.Routes = nil
			},
			wantHop:  "target-port",
			wantWord: "declares no ports",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch := healthyChain()
			tc.mutate(&ch)
			r := Trace(ch)

			if r.BrokenAt != tc.wantHop {
				t.Fatalf("broke at %q, want %q\n%s", r.BrokenAt, tc.wantHop, RenderTrace(r))
			}

			var broken []string
			for _, h := range r.Hops {
				if h.Status == hopBroken {
					broken = append(broken, h.Step)
				}
			}
			if tc.wantHop == "" {
				if len(broken) != 0 {
					t.Errorf("intact chain reported breaks at %v", broken)
				}
				return
			}
			// Exactly one. Downstream consequences must be skipped, not re-reported.
			if len(broken) != 1 {
				t.Errorf("one fault reported as %d breaks: %v\n%s", len(broken), broken, RenderTrace(r))
			}
			for _, h := range r.Hops {
				if h.Step != tc.wantHop {
					continue
				}
				if !strings.Contains(h.Detail, tc.wantWord) {
					t.Errorf("detail did not mention %q:\n  %s", tc.wantWord, h.Detail)
				}
				if h.Remedy == "" {
					t.Errorf("broken hop %q has no remedy; naming a fault without a fix is half an answer", h.Step)
				}
			}
		})
	}
}

// TestTraceSuppressesDownstreamHops pins the suppression rule directly: a selector that matches
// nothing guarantees no endpoints and nothing ready, and those must not read as separate faults.
func TestTraceSuppressesDownstreamHops(t *testing.T) {
	ch := healthyChain()
	ch.Selector = map[string]string{"app": "typo"}
	ch.Matched, ch.EndpointPorts = nil, nil
	ch.EndpointsReady = 0

	r := Trace(ch)
	for _, h := range r.Hops {
		switch h.Step {
		case "selector":
			if h.Status != hopBroken {
				t.Errorf("selector status = %q, want broken", h.Status)
			}
		case "endpoints", "readiness":
			if h.Status != hopSkipped {
				t.Errorf("%s status = %q, want skipped — it is a consequence of the selector break",
					h.Step, h.Status)
			}
			if !strings.Contains(h.Detail, "selector") {
				t.Errorf("%s should point back at the real break, said: %s", h.Step, h.Detail)
			}
		}
	}
}

// TestTraceAlwaysNamesItsGaps guards the property that makes an intact chain useful. "Every hop
// is healthy" with nothing to check next is a dead end, and the intact chain is a common result.
func TestTraceAlwaysNamesItsGaps(t *testing.T) {
	for _, ch := range []model.ServiceChain{healthyChain(), {Service: "bare", Namespace: "x"}} {
		r := Trace(ch)
		if len(r.NotChecked) < 5 {
			t.Errorf("%s: only %d gaps named; the causes outside the declared chain are the "+
				"whole value of an intact result", ch.Service, len(r.NotChecked))
		}
		out := RenderTrace(r)
		for _, want := range []string{"NetworkPolicy", "listening"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: rendered trace never mentions %q", ch.Service, want)
			}
		}
	}
}

// TestTraceNotCheckedLeadsWithExternalPolicy: when the Service demonstrably sets
// externalTrafficPolicy: Local, that gap is not hypothetical and belongs first.
func TestTraceNotCheckedLeadsWithExternalPolicy(t *testing.T) {
	ch := healthyChain()
	ch.ExternalPolicyLocal = true
	got := TraceNotChecked(ch)
	if !strings.Contains(got[0], "externalTrafficPolicy") {
		t.Errorf("first gap = %q, want the externalTrafficPolicy one", got[0])
	}
	if n := len(TraceNotChecked(healthyChain())); len(got) != n+1 {
		t.Errorf("gap count %d, want %d — the line should be added, not substituted", len(got), n+1)
	}
}

// TestTraceIsDeterministic: same rule as the detectors. Port and label lists are built from maps
// and slices across several pods, which is exactly where ordering flaps.
func TestTraceIsDeterministic(t *testing.T) {
	ch := healthyChain()
	ch.Selector = map[string]string{"app": "checkout-api", "tier": "web", "env": "prod"}
	ch.Matched = []model.ChainPod{
		{Name: "b", PortNames: []string{"metrics", "http"}, PortNumbers: []int32{9090, 8080}},
		{Name: "a", PortNames: []string{"http", "admin"}, PortNumbers: []int32{8080, 7000}},
	}
	first := RenderTrace(Trace(ch))
	for i := 0; i < 20; i++ {
		if got := RenderTrace(Trace(ch)); got != first {
			t.Fatalf("run %d differs:\n--- first ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}
}

// TestTraceSelectorlessServiceIsNotAFault: manually managed Endpoints and ExternalName are
// legitimate patterns, and reporting them as broken would be a false positive on a working setup.
func TestTraceSelectorlessServiceIsNotAFault(t *testing.T) {
	ch := healthyChain()
	ch.Selector, ch.Matched = nil, nil
	ch.EndpointsReady = 2 // endpoints managed by hand

	r := Trace(ch)
	if r.BrokenAt != "" {
		t.Errorf("selector-less Service with ready endpoints broke at %q:\n%s", r.BrokenAt, RenderTrace(r))
	}
}

// TestTraceHedgesOnATruncatedPodList: a bounded pod list is fine, but it turns "the selector
// matches nothing" from an incomplete answer into a possibly wrong one, and the hop must say so.
func TestTraceHedgesOnATruncatedPodList(t *testing.T) {
	for _, nearMiss := range [][]string{nil, {"almost-right-0"}} {
		ch := healthyChain()
		ch.Selector = map[string]string{"app": "nothing-here"}
		ch.Matched, ch.EndpointPorts = nil, nil
		ch.EndpointsReady = 0
		ch.NearMiss, ch.NearMissTotal = nearMiss, len(nearMiss)
		ch.PodsTruncated = true

		var detail string
		for _, h := range Trace(ch).Hops {
			if h.Step == "selector" {
				detail = h.Detail
			}
		}
		if !strings.Contains(detail, "not conclusive") {
			t.Errorf("near_miss=%v: a truncated list produced a definitive verdict:\n  %s",
				nearMiss, detail)
		}
	}
}

// TestTraceRemedyNamesTheFailingRoutesPortKind: a manifest can mix a numbered and a named backend
// port, and the remedy previously described whichever route came first rather than the broken one.
func TestTraceRemedyNamesTheFailingRoutesPortKind(t *testing.T) {
	ch := healthyChain()
	ch.Routes = []model.IngressRoute{
		{Ingress: "ok", Host: "a", Path: "/", BackendPort: "80"},                         // valid, numeric
		{Ingress: "bad", Host: "b", Path: "/", BackendPort: "grpc", BackendIsName: true}, // broken, named
	}
	ch.Ports = []model.ServicePortSpec{
		{Name: "http", Port: 80, TargetPort: "http", TargetIsName: true},
	}
	for _, h := range Trace(ch).Hops {
		if h.Step != "ingress" {
			continue
		}
		if h.Status != hopBroken {
			t.Fatalf("ingress status = %q, want broken", h.Status)
		}
		if !strings.Contains(h.Remedy, "backend port name") {
			t.Errorf("remedy described the wrong port kind — the FAILING route names its port:\n  %s",
				h.Remedy)
		}
	}
}
