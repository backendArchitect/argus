package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/backendArchitect/argus/internal/detect"
	"github.com/backendArchitect/argus/internal/kube"
	"github.com/backendArchitect/argus/internal/model"
)

type triageIn struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Namespace to scan. Omit to scan the whole cluster."`
}

func registerTriage(s *mcp.Server, opts kube.Options) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "cluster_triage",
		Description: "Find what is broken right now, across a namespace or the whole cluster. " +
			"Results are grouped by controller, never by pod — forty crashlooping pods of one " +
			"Deployment is one entry with a count. Start here when you do not yet know which " +
			"workload to ask about, then call diagnose_workload on anything it surfaces.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in triageIn) (*mcp.CallToolResult, *model.TriageResult, error) {
		c, err := kube.New(opts)
		if err != nil {
			return nil, nil, fmt.Errorf("cluster access: %w", err)
		}
		snaps, degraded, notes, err := c.Triage(ctx, in.Namespace)
		if err != nil {
			return nil, nil, err
		}
		scope := "cluster"
		if in.Namespace != "" {
			scope = "namespace/" + in.Namespace
		}
		res := detect.Triage(scope, snaps, degraded, notes)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: detect.UntrustedNote + "\n" + detect.RenderTriage(res),
		}}}, res, nil
	})
}
