package model

import (
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Snapshot is everything argus gathered about one question, projected down to an explicit
// allowlist of fields. Detectors are pure functions over this type.
//
// Time is stored as *seconds ago* rather than as absolute timestamps. A fixture with absolute
// times silently rots: a detector asking "was this OOMKill recent?" stops firing the day after the
// fixture is captured, and the test still passes because no detector fires and none was expected.
// Relative time keeps committed fixtures meaningful forever.
type Snapshot struct {
	Scope     string `json:"scope"`     // "workload/prod/checkout-api" or "cluster"
	Namespace string `json:"namespace"` // empty for cluster scope

	Workload    *WorkloadView    `json:"workload,omitempty"`
	ReplicaSets []ReplicaSetView `json:"replicasets,omitempty"`
	Pods        []PodView        `json:"pods,omitempty"`
	Events      []EventGroup     `json:"events,omitempty"`
	Services    []ServiceView    `json:"services,omitempty"`
	Nodes       []NodeView       `json:"nodes,omitempty"`
	HPA         *HPAView         `json:"hpa,omitempty"`
	PDB         *PDBView         `json:"pdb,omitempty"`

	// Degraded lists gather steps that failed or timed out. Detectors working from partial data
	// must dock their confidence — see Snapshot.Missing.
	Degraded []string `json:"degraded,omitempty"`

	// Notes records deliberate elisions — data we chose not to keep, not data we failed to get.
	// Kept separate from Degraded so that trimming rollout history never makes a detector think
	// the apiserver was unreachable. Silent truncation reads as "we looked at everything".
	Notes []string `json:"notes,omitempty"`
}

// Missing reports whether a named gather step failed, so a detector can lower its confidence
// rather than silently reason from absent data.
func (s *Snapshot) Missing(step string) bool {
	for _, d := range s.Degraded {
		if strings.HasPrefix(d, step) {
			return true
		}
	}
	return false
}

// WorkloadView is the controller under diagnosis.
type WorkloadView struct {
	Kind      string            `json:"kind"` // Deployment | StatefulSet | DaemonSet | Rollout
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels,omitempty"`
	Selector  map[string]string `json:"selector,omitempty"`

	Desired   int32 `json:"desired"`
	Ready     int32 `json:"ready"`
	Updated   int32 `json:"updated"`
	Available int32 `json:"available"`

	Generation         int64 `json:"generation"`
	ObservedGeneration int64 `json:"observed_generation"`
	CreatedSecondsAgo  int64 `json:"created_seconds_ago"`

	Conditions []ConditionView `json:"conditions,omitempty"`
}

// ReplicaSetView carries enough of a ReplicaSet to diff two generations and tell which one is the
// current rollout target.
type ReplicaSetView struct {
	Name              string `json:"name"`
	Revision          string `json:"revision,omitempty"`
	Desired           int32  `json:"desired"`
	Ready             int32  `json:"ready"`
	Available         int32  `json:"available"`
	Current           bool   `json:"current"` // matches the workload's current pod-template hash
	CreatedSecondsAgo int64  `json:"created_seconds_ago"`

	// Template is the projected pod template, which is what the rollout detector diffs.
	Template []ContainerSpecView `json:"template,omitempty"`
}

// PodView is the projected pod. Budget: under 400 tokens serialized — enforced by test.
type PodView struct {
	Name      string            `json:"name"`
	Node      string            `json:"node,omitempty"`
	Phase     string            `json:"phase"`
	Ready     bool              `json:"ready"`
	Labels    map[string]string `json:"labels,omitempty"`
	OwnerKind string            `json:"owner_kind,omitempty"`
	OwnerName string            `json:"owner_name,omitempty"`

	CreatedSecondsAgo int64 `json:"created_seconds_ago"`

	// SchedulingReason/Message come from the PodScheduled condition when Phase is Pending.
	SchedulingReason  string `json:"scheduling_reason,omitempty"`
	SchedulingMessage string `json:"scheduling_message,omitempty"`

	Containers []ContainerView `json:"containers,omitempty"`
}

// ContainerView merges the container spec, its live status, and its current metrics — the three
// things every detector needs to correlate and which kubectl makes you fetch separately.
type ContainerView struct {
	ContainerSpecView `json:",inline"`

	Ready        bool  `json:"ready"`
	Started      bool  `json:"started"`
	RestartCount int32 `json:"restart_count"`

	State     ContainerStateView  `json:"state"`
	LastState *ContainerStateView `json:"last_state,omitempty"`

	// UsageCPU/UsageMem come from metrics.k8s.io; empty when the metrics API is unavailable.
	UsageCPU string `json:"usage_cpu,omitempty"`
	UsageMem string `json:"usage_mem,omitempty"`
}

// ContainerSpecView is the desired shape of a container — the half of ContainerView that also
// appears in a ReplicaSet template, which is why it is factored out and diffable on its own.
type ContainerSpecView struct {
	Name  string `json:"name"`
	Image string `json:"image"`

	RequestCPU string `json:"request_cpu,omitempty"`
	RequestMem string `json:"request_mem,omitempty"`
	LimitCPU   string `json:"limit_cpu,omitempty"`
	LimitMem   string `json:"limit_mem,omitempty"`

	Args    []string `json:"args,omitempty"`
	EnvKeys []string `json:"env_keys,omitempty"` // keys only; values are redacted at the projection boundary
	EnvFrom []string `json:"env_from,omitempty"`
	Mounts  []string `json:"mounts,omitempty"`

	Readiness *ProbeView `json:"readiness,omitempty"`
	Liveness  *ProbeView `json:"liveness,omitempty"`
	Startup   *ProbeView `json:"startup,omitempty"`
}

// ProbeView is the probe timing a detector reasons about; the handler details do not matter to it.
type ProbeView struct {
	Kind             string `json:"kind"` // http | tcp | exec | grpc
	InitialDelay     int32  `json:"initial_delay"`
	Period           int32  `json:"period"`
	Timeout          int32  `json:"timeout"`
	FailureThreshold int32  `json:"failure_threshold"`
	SuccessThreshold int32  `json:"success_threshold,omitempty"`
}

// Deadline is how long the probe tolerates a slow start before the kubelet acts, in seconds.
// This is the number the readiness detector compares against observed startup time — computing it
// by hand from four separate fields is exactly the arithmetic humans get wrong at 3am.
func (p *ProbeView) Deadline() int32 {
	if p == nil {
		return 0
	}
	return p.InitialDelay + p.Period*p.FailureThreshold
}

// ContainerStateView flattens running/waiting/terminated into one shape.
type ContainerStateView struct {
	Status     string `json:"status"` // running | waiting | terminated
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	ExitCode   int32  `json:"exit_code,omitempty"`
	Signal     int32  `json:"signal,omitempty"`
	SecondsAgo int64  `json:"seconds_ago,omitempty"` // since started or finished
}

// EventGroup is a deduplicated bucket of events. A hundred BackOff events collapse to one line.
type EventGroup struct {
	Type       string `json:"type"` // Normal | Warning
	Reason     string `json:"reason"`
	Message    string `json:"message"` // normalized: UIDs, IPs, digests stripped
	Count      int32  `json:"count"`   // total occurrences, respecting the apiserver's own aggregation
	ObjectKind string `json:"object_kind,omitempty"`
	ObjectName string `json:"object_name,omitempty"` // one example object, not the only one
	// ObjectCount is how many distinct objects reported this. Forty pods reporting the same
	// BackOff is one group with ObjectCount 40, which is the fact an SRE actually wants.
	ObjectCount         int   `json:"object_count,omitempty"`
	FirstSeenSecondsAgo int64 `json:"first_seen_seconds_ago"`
	LastSeenSecondsAgo  int64 `json:"last_seen_seconds_ago"`
}

// ServiceView plus its endpoint readiness — the pair that exposes the classic silent failure where
// a Service selector matches nothing and `kubectl get` looks entirely healthy.
type ServiceView struct {
	Name          string            `json:"name"`
	Selector      map[string]string `json:"selector,omitempty"`
	Ports         []string          `json:"ports,omitempty"`
	ReadyCount    int               `json:"ready_count"`
	NotReadyCount int               `json:"not_ready_count"`
	// MatchedPods is how many pods in the namespace the selector matches at all, ready or not.
	// Zero here means a label mismatch; non-zero with ReadyCount 0 means a readiness failure.
	MatchedPods int `json:"matched_pods"`
}

// NodeView carries the conditions that explain workload failures the workload did not cause.
type NodeView struct {
	Name          string          `json:"name"`
	Ready         bool            `json:"ready"`
	Unschedulable bool            `json:"unschedulable,omitempty"`
	Conditions    []ConditionView `json:"conditions,omitempty"`
	Taints        []string        `json:"taints,omitempty"`
	AllocCPU      string          `json:"alloc_cpu,omitempty"`
	AllocMem      string          `json:"alloc_mem,omitempty"`
}

// ConditionView is a k8s condition reduced to what a detector reads.
type ConditionView struct {
	Type              string `json:"type"`
	Status            string `json:"status"`
	Reason            string `json:"reason,omitempty"`
	Message           string `json:"message,omitempty"`
	LastChangeSecsAgo int64  `json:"last_change_seconds_ago,omitempty"`
}

// HPAView explains scale decisions that look like workload failures.
type HPAView struct {
	Name        string          `json:"name"`
	MinReplicas int32           `json:"min_replicas"`
	MaxReplicas int32           `json:"max_replicas"`
	Current     int32           `json:"current_replicas"`
	Desired     int32           `json:"desired_replicas"`
	Conditions  []ConditionView `json:"conditions,omitempty"`
}

// PDBView explains rollouts that are stuck rather than broken.
type PDBView struct {
	Name               string `json:"name"`
	DesiredHealthy     int32  `json:"desired_healthy"`
	CurrentHealthy     int32  `json:"current_healthy"`
	DisruptionsAllowed int32  `json:"disruptions_allowed"`
}

// ParseCPU returns a CPU quantity in millicores, or 0 if unset/unparseable.
// Detectors do arithmetic on these, so a bad value must degrade to "unknown", never panic.
func ParseCPU(q string) int64 {
	if q == "" {
		return 0
	}
	v, err := resource.ParseQuantity(q)
	if err != nil {
		return 0
	}
	return v.MilliValue()
}

// ParseMem returns a memory quantity in bytes, or 0 if unset/unparseable.
func ParseMem(q string) int64 {
	if q == "" {
		return 0
	}
	v, err := resource.ParseQuantity(q)
	if err != nil {
		return 0
	}
	return v.Value()
}

// LogBundle is projected container output: redacted, grouped and budgeted.
//
// Raw logs are the least structured and most dangerous thing argus emits — unbounded in size, and
// the one place an application can print a credential. Everything here exists to bound one of
// those two risks.
type LogBundle struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Previous reports whether these are the *previous* container instance's logs. On a
	// crashlooping pod that is almost always what you want: the current instance is in backoff
	// and has produced nothing.
	Previous bool `json:"previous"`
	// Reason explains why this pod, container and instance were chosen, so the selection is
	// auditable rather than magic.
	Reason string `json:"reason"`

	Groups []LogGroup `json:"groups"`

	// DroppedGroups counts distinct lines elided by the token budget. Never silent: a truncated
	// log that does not say it was truncated reads as a complete one.
	DroppedGroups int    `json:"dropped_groups,omitempty"`
	Note          string `json:"note,omitempty"`
}

// LogGroup is a set of log lines identical after normalization. Ten thousand identical panics
// collapse to one entry with a count, which is the difference between a readable diagnosis and a
// context window full of the same stack trace.
type LogGroup struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
	// FirstSecondsAgo/LastSecondsAgo are zero when the log carried no usable timestamps.
	FirstSecondsAgo int64 `json:"first_seconds_ago,omitempty"`
	LastSecondsAgo  int64 `json:"last_seconds_ago,omitempty"`
}
