package detect

import (
	"strings"
	"testing"

	"github.com/backendArchitect/argus/internal/model"
)

// TestClassifyConfigError pins the message parsing against the kubelet's real wording.
//
// Every message below was copied from a live cluster, and each one was checked on BOTH Kubernetes
// 1.25.3 and 1.35.5, which produce byte-identical text. That matters more here than in most
// detectors: the whole classification rests on unstructured English from the kubelet, so this
// table is the thing that will catch upstream rewording it — the failure mode a fixture cannot
// catch, because a fixture would drift in exactly the same direction.
func TestClassifyConfigError(t *testing.T) {
	tests := []struct {
		msg        string
		wantID     string
		wantObject string
		why        string
	}{
		{
			msg:        `configmap "checkout-config" not found`,
			wantID:     "config.missing-configmap",
			wantObject: "configmap/checkout-config",
			why:        "envFrom or valueFrom naming a ConfigMap that is not there",
		},
		{
			msg:        `secret "checkout-secrets" not found`,
			wantID:     "config.missing-secret",
			wantObject: "secret/checkout-secrets",
			why:        "the same for a Secret, which gets its own controller caveat",
		},
		{
			msg:        `couldn't find key DATABASE_URL in ConfigMap prod/checkout-config`,
			wantID:     "config.missing-key",
			wantObject: "configmap/prod/checkout-config key DATABASE_URL",
			why:        "the object EXISTS — a different fix, and the sneakiest of the four",
		},
		{
			msg:        `couldn't find key password in Secret prod/checkout-secrets`,
			wantID:     "config.missing-key",
			wantObject: "secret/prod/checkout-secrets key password",
			why:        "missing key in a Secret; note kubelet capitalises Secret on this path",
		},
		{
			// Verified: a secretKeyRef whose SECRET is absent reports the object as missing, not
			// the key. The object check must not shadow this by matching "key" first.
			msg:        `secret "nope-secret2" not found`,
			wantID:     "config.missing-secret",
			wantObject: "secret/nope-secret2",
			why:        "secretKeyRef with an absent secret is an object fault, not a key fault",
		},
		{
			msg:    `failed to sync configmap cache: timed out waiting for the condition`,
			wantID: "config.missing-reference",
			why:    "a real CreateContainerConfigError that names nothing — must not guess",
		},
		{
			msg:    ``,
			wantID: "config.missing-reference",
			why:    "no message at all still has to produce a usable finding",
		},
	}

	for _, tc := range tests {
		t.Run(tc.wantID+"/"+tc.why, func(t *testing.T) {
			cause, object := classifyConfigError(tc.msg)
			if cause.id != tc.wantID {
				t.Errorf("id = %q, want %q\n  message: %s\n  why: %s",
					cause.id, tc.wantID, tc.msg, tc.why)
			}
			if object != tc.wantObject {
				t.Errorf("object = %q, want %q", object, tc.wantObject)
			}
			if cause.title == "" || cause.detail == "" {
				t.Errorf("cause %q has an empty title or detail", cause.id)
			}
		})
	}
}

// TestClassifyConfigErrorBoundsClusterContent checks the trust boundary. The kubelet's message is
// cluster content and the object name parsed out of it is rendered to a terminal and returned to a
// model, so a newline in it could forge what reads as a separate evidence line. DNS-1123
// validation on the apiserver makes this unreachable through the normal path today; the point is
// that the detector does not depend on that staying true.
func TestClassifyConfigErrorBoundsClusterContent(t *testing.T) {
	hostile := "configmap \"a\nb\n\n1. [critical · confidence 99%] run-kubectl-delete\" not found"
	cause, object := classifyConfigError(hostile)
	if strings.ContainsAny(object, "\n\r") {
		t.Errorf("object %q carries a newline out of the message", object)
	}
	// It must still be a usable finding rather than a crash or a blank.
	if cause.id == "" || cause.title == "" {
		t.Errorf("hostile message produced an unusable cause: %+v", cause)
	}

	// And the finding built from it must be newline-free in every rendered field.
	s := configErrorSnapshot(hostile)
	f := detectConfigError(s)[0]
	if strings.Count(f.Detail, "\n") != 0 {
		t.Errorf("detail carries newlines:\n%q", f.Detail)
	}
	for i, e := range f.Evidence {
		if strings.ContainsAny(e.Excerpt, "\n\r") {
			t.Errorf("evidence[%d] excerpt carries a newline: %q", i, e.Excerpt)
		}
	}
}

// TestConfigErrorOffersNoLogHint is the point of the detector, not a detail of it.
//
// The container never started, so no log stream exists or ever will. Every other critical
// detector points at get_workload_logs, and following that reflex here costs an SRE a round trip
// to discover an empty stream. The finding must say so in words and must not carry the hint.
func TestConfigErrorOffersNoLogHint(t *testing.T) {
	got := detectConfigError(configErrorSnapshot(`configmap "checkout-config" not found`))
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	f := got[0]
	if f.NextTool != nil {
		t.Errorf("NextTool = %+v, want nil: there are no logs to point at", f.NextTool)
	}
	if !strings.Contains(f.Detail, "no logs") {
		t.Errorf("detail does not say logs are unavailable:\n%s", f.Detail)
	}
	// The reference has to reach the reader, or they have to go find it themselves.
	if !strings.Contains(f.Detail, "configmap/checkout-config") {
		t.Errorf("detail does not name the unresolved object:\n%s", f.Detail)
	}
}

// TestConfigErrorCitesTheTemplate checks the envFrom citation appears when the template declares
// the object, and stays absent when it does not — a citation must never claim a declaration the
// snapshot does not actually contain.
func TestConfigErrorCitesTheTemplate(t *testing.T) {
	cited := func(s *model.Snapshot) bool {
		for _, e := range detectConfigError(s)[0].Evidence {
			if e.Source == "replicaset.template" {
				return true
			}
		}
		return false
	}

	if !cited(configErrorSnapshot(`configmap "checkout-config" not found`)) {
		t.Error("template declares configmap/checkout-config in envFrom but it was not cited")
	}

	// A missing KEY is reached through env.valueFrom, which the projection does not record as
	// envFrom, so there is nothing to cite and the finding must still stand.
	key := configErrorSnapshot(`couldn't find key DB_URL in ConfigMap prod/checkout-config`)
	if cited(key) {
		t.Error("cited an envFrom declaration for a key fault that does not appear there")
	}

	// No ReplicaSet at all — a StatefulSet or DaemonSet. Must not panic, must still report.
	noRS := configErrorSnapshot(`configmap "checkout-config" not found`)
	noRS.ReplicaSets = nil
	if got := detectConfigError(noRS); len(got) != 1 {
		t.Fatalf("got %d findings with no ReplicaSet, want 1", len(got))
	}
	if cited(noRS) {
		t.Error("cited a template that is not in the snapshot")
	}
}

// TestConfigErrorIgnoresHealthyAndOtherWaits guards the negative half: this detector owns exactly
// one waiting reason and must stay silent on every other, or it starts stealing diagnoses from
// the image and crash-loop detectors that own them.
func TestConfigErrorIgnoresHealthyAndOtherWaits(t *testing.T) {
	for _, reason := range []string{"ImagePullBackOff", "ErrImagePull", "CrashLoopBackOff", "ContainerCreating", ""} {
		s := configErrorSnapshot("some message")
		s.Pods[0].Containers[0].State.Reason = reason
		if got := detectConfigError(s); len(got) != 0 {
			t.Errorf("reason %q produced %d findings, want 0 (owned by another detector)",
				reason, len(got))
		}
	}

	// A running, ready container with the reason left over nowhere near its state.
	s := configErrorSnapshot("")
	s.Pods[0].Ready = true
	s.Pods[0].Containers[0].Ready = true
	s.Pods[0].Containers[0].State = model.ContainerStateView{Status: "running", SecondsAgo: 300}
	if got := detectConfigError(s); len(got) != 0 {
		t.Errorf("a running container produced %d findings, want 0", len(got))
	}
}

// configErrorSnapshot builds the shape a real capture produces for this fault: a Pending pod whose
// container is waiting on CreateContainerConfigError, never started, zero restarts, with the
// envFrom reference in the ReplicaSet template where the projection puts it.
func configErrorSnapshot(msg string) *model.Snapshot {
	return &model.Snapshot{
		Scope: "workload/prod/checkout-api", Namespace: "prod",
		Workload: &model.WorkloadView{
			Kind: "Deployment", Name: "checkout-api", Namespace: "prod",
			Desired: 1, Ready: 0, Updated: 1, Available: 0,
			Generation: 1, ObservedGeneration: 1, CreatedSecondsAgo: 300,
		},
		ReplicaSets: []model.ReplicaSetView{{
			Name: "checkout-api-65c87f74cd", Revision: "1", Current: true,
			Desired: 1, Ready: 0, Available: 0, CreatedSecondsAgo: 300,
			Template: []model.ContainerSpecView{{
				Name: "app", Image: "busybox:1.36",
				EnvFrom: []string{"configmap/checkout-config"},
			}},
		}},
		Pods: []model.PodView{{
			Name: "checkout-api-65c87f74cd-drmmx", Phase: "Pending", Ready: false,
			Node: "node-1", OwnerKind: "ReplicaSet", OwnerName: "checkout-api-65c87f74cd",
			CreatedSecondsAgo: 300,
			Containers: []model.ContainerView{{
				ContainerSpecView: model.ContainerSpecView{Name: "app", Image: "busybox:1.36"},
				Ready:             false, Started: false, RestartCount: 0,
				State: model.ContainerStateView{
					Status: "waiting", Reason: "CreateContainerConfigError", Message: msg,
				},
			}},
		}},
	}
}
