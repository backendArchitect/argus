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

type pendingIn struct {
	Workload  string `json:"workload" jsonschema:"Workload name. Fuzzy: 'checkout' or 'deploy/checkout-api' both work."`
	Namespace string `json:"namespace" jsonschema:"Namespace to look in. Required."`
}

type pendingOut struct {
	Scope   string                 `json:"scope"`
	Reports []*model.PendingReport `json:"reports" jsonschema:"one per pending pod; empty when everything is scheduled"`
	Calls   int64                  `json:"apiserver_calls"`
}

func registerPending(s *mcp.Server, opts kube.Options) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "explain_pending",
		Description: "Explain why a workload's pods will not schedule. For every node, reports " +
			"whether the pod could be placed there and if not why, with the actual numbers: what " +
			"the pod asked for, what the node has, and how much is already committed to other " +
			"pods. Also states which constraints it does NOT evaluate, so a gap is never mistaken " +
			"for a clean bill of health.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in pendingIn) (*mcp.CallToolResult, pendingOut, error) {
		if in.Workload == "" || in.Namespace == "" {
			return nil, pendingOut{}, errors.New("both workload and namespace are required")
		}
		c, err := kube.New(opts)
		if err != nil {
			return nil, pendingOut{}, fmt.Errorf("cluster access: %w", err)
		}
		ref, err := c.Resolve(ctx, in.Workload, in.Namespace)
		if err != nil {
			var amb *kube.AmbiguousError
			if errors.As(err, &amb) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: amb.Error()}}},
					pendingOut{}, nil
			}
			return nil, pendingOut{}, err
		}
		reports, err := c.Pending(ctx, ref)
		if err != nil {
			return nil, pendingOut{}, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: detect.RenderPending(reports),
		}}}, pendingOut{Scope: ref.String(), Reports: reports, Calls: c.Calls()}, nil
	})
}
