package model

// The service path as plain data: the chain from arriving traffic to a container that could
// serve it.
//
// Traced hop by hop, because the useful answer is WHERE it breaks. Ranking would be the wrong
// shape here — the first break is the cause, and every hop past it is unreachable rather than
// healthy. Reporting all three of "selector matches nothing", "no endpoints" and "nothing ready"
// describes one fault three times.

// ServicePortSpec is one Service port and the target it resolves to.
type ServicePortSpec struct {
	Name     string `json:"name,omitempty"`
	Port     int32  `json:"port"`
	Protocol string `json:"protocol,omitempty"`
	// TargetPort as written. A NAME here is the case worth tracing: it has to match a
	// containerPort name, and when it does not, the Service produces no endpoint port at
	// all while both the Service and the pods keep reporting healthy.
	TargetPort   string `json:"target_port"`
	TargetIsName bool   `json:"target_is_name,omitempty"`
}

// ChainPod is a pod the selector matched, with the ports its containers declare.
type ChainPod struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	Phase string `json:"phase,omitempty"`
	// PortNames and PortNumbers are what the containers DECLARE. Kubernetes neither
	// requires a listening port to be declared nor verifies that a declared one is
	// listening, which is why a numeric mismatch is a hint and a named one is proof.
	PortNames   []string `json:"port_names,omitempty"`
	PortNumbers []int32  `json:"port_numbers,omitempty"`
}

// IngressRoute is one Ingress rule pointing at the Service being traced.
type IngressRoute struct {
	Ingress string `json:"ingress"`
	Class   string `json:"class,omitempty"`
	Host    string `json:"host,omitempty"`
	Path    string `json:"path,omitempty"`
	// BackendPort as written on the rule: a number, or a Service port NAME.
	BackendPort   string `json:"backend_port"`
	BackendIsName bool   `json:"backend_is_name,omitempty"`
}

// ServiceChain is the input to a trace. Plain data with no client and no live objects, so
// Trace stays a pure function and is testable without a cluster, like every detector.
type ServiceChain struct {
	Service   string            `json:"service"`
	Namespace string            `json:"namespace"`
	Type      string            `json:"type,omitempty"`
	Selector  map[string]string `json:"selector,omitempty"`
	Ports     []ServicePortSpec `json:"ports,omitempty"`
	Routes    []IngressRoute    `json:"routes,omitempty"`
	Matched   []ChainPod        `json:"matched_pods,omitempty"`

	// NearMiss names pods whose label value is a plausible misspelling of what the selector
	// wants. That is the shape a mislabelled workload has, and it is the whole difference
	// between "nothing is deployed here" and "your selector has a typo".
	//
	// Plausible is load-bearing. Matching on a shared label KEY looks equivalent and
	// over-matches badly: every pod in a namespace carries `app`, so on a live cluster it
	// named six unrelated workloads and pointed the reader at the wrong one.
	NearMiss []string `json:"near_miss_pods,omitempty"`
	// NearMissTotal is how many pods qualified, which is not len(NearMiss) — that list is
	// truncated for readability, and reporting its length as the count states a wrong number.
	NearMissTotal int `json:"near_miss_total,omitempty"`

	EndpointsReady    int `json:"endpoints_ready"`
	EndpointsNotReady int `json:"endpoints_not_ready"`
	// EndpointPorts is what the EndpointSlices actually carry. Empty while pods matched is
	// the dataplane confirming a targetPort that resolved to nothing.
	EndpointPorts []int32 `json:"endpoint_ports,omitempty"`

	ExternalPolicyLocal bool `json:"external_traffic_policy_local,omitempty"`

	// PodsTruncated records that the namespace holds more pods than were listed. It matters
	// because it turns "the selector matches nothing" from an incomplete answer into a
	// possibly wrong one, and that hop has to hedge rather than stay definitive.
	PodsTruncated bool `json:"pods_truncated,omitempty"`
}

// Hop is one link in the chain, in the order traffic traverses it.
type Hop struct {
	Step string `json:"step"`
	// Status is ok | broken | warn | skipped. "skipped" covers both a hop that does not
	// apply and one downstream of a break, where the data says nothing about health.
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Remedy is set only on the hop where the chain breaks.
	Remedy string `json:"remedy,omitempty"`
}

// TraceReport is the answer: the chain, and where it gives out.
type TraceReport struct {
	Service   string `json:"service"`
	Namespace string `json:"namespace"`
	Hops      []Hop  `json:"hops"`
	// BrokenAt names the first failing hop, empty when the declared chain is intact.
	BrokenAt string `json:"broken_at,omitempty"`
	// NotChecked is load-bearing for this tool in a way it is not for the others. The
	// declared chain being intact is a common and genuinely useful result, and it means the
	// cause is in here — so an empty or vague list would send the reader back to what they
	// already ruled out.
	NotChecked []string `json:"not_checked"`
}
