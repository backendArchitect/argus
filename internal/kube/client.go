// Package kube talks to the apiserver and projects what it finds into model.Snapshot.
//
// Everything here is read-only. That is not a convention: TestNoMutatingVerbs walks this package's
// AST and fails on any call to a mutating client verb. v0.1 runs under a developer kubeconfig with
// full cluster-admin, so RBAC will not hold that line for us — the binary has to.
package kube

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

// Options configures cluster access and the blast-radius limits on a single tool invocation.
type Options struct {
	Kubeconfig string        // path; empty means the standard loading rules, then in-cluster
	Context    string        // kubeconfig context name; empty means current-context
	Timeout    time.Duration // per-invocation deadline for the whole gather
	MaxCalls   int64         // hard cap on apiserver requests per invocation
}

// DefaultOptions are deliberately conservative. A diagnostic tool that DoSes the control plane
// during an incident is a career-limiting artifact.
func DefaultOptions() Options {
	return Options{Timeout: 10 * time.Second, MaxCalls: 60}
}

// Client bundles the three typed clients a gather needs, plus the call budget they share.
type Client struct {
	Typed   kubernetes.Interface
	Metrics metricsv.Interface
	Dynamic dynamic.Interface
	Context string // resolved context name, for audit logging
	opts    Options
	calls   *atomic.Int64
}

// budget is an http.RoundTripper that fails fast once an invocation has spent its call budget.
// Capping at the transport is what makes the limit real: it counts every request from every client
// and every retry, which a hand-rolled counter around call sites would miss.
type budget struct {
	next  http.RoundTripper
	calls *atomic.Int64
	max   int64
}

func (b *budget) RoundTrip(r *http.Request) (*http.Response, error) {
	if n := b.calls.Add(1); n > b.max {
		return nil, fmt.Errorf("apiserver call budget exhausted (%d calls); "+
			"narrow the scope with --namespace or raise --max-calls", b.max)
	}
	return b.next.RoundTrip(r)
}

// New builds a Client from a kubeconfig, falling back to in-cluster config.
func New(opts Options) (*Client, error) {
	if opts.Timeout == 0 {
		opts.Timeout = DefaultOptions().Timeout
	}
	if opts.MaxCalls == 0 {
		opts.MaxCalls = DefaultOptions().MaxCalls
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		rules.ExplicitPath = opts.Kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: opts.Context})

	cfg, err := cc.ClientConfig()
	if err != nil {
		// clientcmd already tries in-cluster config as part of its loading rules; a failure here
		// means neither path worked, and the kubeconfig error is the more actionable one.
		return nil, fmt.Errorf("no cluster config (kubeconfig or in-cluster): %w", err)
	}

	name := opts.Context
	if name == "" {
		if raw, err := cc.RawConfig(); err == nil {
			name = raw.CurrentContext
		}
	}

	calls := &atomic.Int64{}
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return &budget{next: rt, calls: calls, max: opts.MaxCalls}
	}
	// QPS defaults throttle client-side at 5/s, which serializes a fan-out that is already capped
	// by MaxCalls. Raise it so the budget, not the rate limiter, is the binding constraint.
	cfg.QPS = 50
	cfg.Burst = 100

	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("typed client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	// The metrics API is an aggregated APIService and is frequently absent. Construction failing
	// here is fatal, but a call failing later is only Degraded — see gather.
	mc, err := metricsv.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("metrics client: %w", err)
	}

	return &Client{Typed: typed, Metrics: mc, Dynamic: dyn, Context: name, opts: opts, calls: calls}, nil
}

// Calls reports apiserver requests spent so far, for the audit log.
func (c *Client) Calls() int64 { return c.calls.Load() }

// WithTimeout derives the per-invocation deadline. Callers must use the returned context for every
// apiserver call so a slow cluster degrades the snapshot instead of hanging the MCP session.
func (c *Client) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.opts.Timeout)
}
