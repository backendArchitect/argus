package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/backendArchitect/argus/internal/detect"
	"github.com/backendArchitect/argus/internal/kube"
	"github.com/backendArchitect/argus/internal/model"
)

type diagnoseIn struct {
	Workload  string `json:"workload" jsonschema:"Workload name. Fuzzy: 'checkout' or 'checkout-api' both work, and it may be kind-qualified as deploy/checkout-api, sts/…, ds/… or rollout/…"`
	Namespace string `json:"namespace" jsonschema:"Namespace to look in. Required."`
}

type diagnoseOut struct {
	Scope      string          `json:"scope" jsonschema:"what was diagnosed"`
	Findings   []model.Finding `json:"findings" jsonschema:"ranked worst-first; each carries evidence and an honest confidence"`
	Total      int             `json:"total_findings" jsonschema:"total found, which may exceed the number returned"`
	Degraded   []string        `json:"degraded,omitempty" jsonschema:"lookups that failed; findings relying on them have reduced confidence"`
	Notes      []string        `json:"notes,omitempty" jsonschema:"data deliberately elided"`
	Calls      int64           `json:"apiserver_calls" jsonschema:"apiserver requests this diagnosis cost"`
	Candidates []string        `json:"candidates,omitempty" jsonschema:"set when the workload name was ambiguous; nothing was diagnosed"`
}

func registerDiagnose(s *mcp.Server, opts kube.Options) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "diagnose_workload",
		Description: "Diagnose why a Kubernetes workload is unhealthy. Returns a ranked list of " +
			"probable causes, each citing the evidence it used and stating an honest confidence. " +
			"Prefer this over fetching pods/events/logs separately: it correlates twelve resource " +
			"kinds in one read-only pass and reports the cause rather than the symptoms.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in diagnoseIn) (*mcp.CallToolResult, diagnoseOut, error) {
		if in.Workload == "" || in.Namespace == "" {
			return nil, diagnoseOut{}, errors.New("both workload and namespace are required")
		}

		c, err := kube.New(opts)
		if err != nil {
			return nil, diagnoseOut{}, fmt.Errorf("cluster access: %w", err)
		}

		ref, err := c.Resolve(ctx, in.Workload, in.Namespace)
		if err != nil {
			// Ambiguity is not an error the model should retry blindly — hand back the
			// candidates so it can pick, rather than making it guess twice.
			var amb *kube.AmbiguousError
			if errors.As(err, &amb) {
				names := make([]string, len(amb.Candidates))
				for i, cand := range amb.Candidates {
					names[i] = cand.String()
				}
				out := diagnoseOut{Scope: in.Namespace + "/" + in.Workload, Candidates: names}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
					Text: fmt.Sprintf("%q matches %d workloads; re-run with one of: %v",
						in.Workload, len(names), names),
				}}}, out, nil
			}
			return nil, diagnoseOut{}, err
		}

		snap, err := c.Gather(ctx, ref)
		if err != nil {
			return nil, diagnoseOut{}, err
		}

		all := detect.All(snap)
		shown := all
		if len(shown) > detect.MaxFindings {
			shown = shown[:detect.MaxFindings]
		}

		out := diagnoseOut{
			Scope:    snap.Scope,
			Findings: shown,
			Total:    len(all),
			Degraded: snap.Degraded,
			Notes:    snap.Notes,
			Calls:    c.Calls(),
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: detect.UntrustedNote + "\n" + detect.Render(snap, all),
		}}}, out, nil
	})
}
