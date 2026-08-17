// Package tools wires the diagnosis pipeline to MCP tools.
//
// Design rule: one tool per SRE question, not one per resource. A resource-shaped surface pushes
// correlation onto the model across many round-trips, which is the failure mode argus exists to fix.
package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/backendArchitect/argus/internal/kube"
)

// Version is the binary version, overridden at build time via -ldflags.
var Version = "0.1.0-dev"

// Server builds the MCP server with every argus tool registered.
//
// The cluster client is constructed per tool call rather than here, so the server
// starts and answers server_info even with no kubeconfig — a broken cluster
// config should surface as one tool erroring, not as a server that will not boot.
func Server(opts kube.Options) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "argus",
		Version: Version,
		Title:   "argus — Kubernetes incident diagnosis",
	}, nil)

	registerServerInfo(s)
	registerDiagnose(s, opts)
	registerLogs(s, opts)
	return s
}

type serverInfoIn struct{}

type serverInfoOut struct {
	Name     string   `json:"name" jsonschema:"server name"`
	Version  string   `json:"version" jsonschema:"server version"`
	ReadOnly bool     `json:"read_only" jsonschema:"whether any mutating call site exists in this binary"`
	Tools    []string `json:"tools" jsonschema:"tools this server exposes"`
}

// registerServerInfo is the handshake canary: it needs no cluster, so a failure here is transport,
// not Kubernetes.
func registerServerInfo(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "server_info",
		Description: "Report argus version, read-only status, and the available tools. Needs no cluster access.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in serverInfoIn) (*mcp.CallToolResult, serverInfoOut, error) {
		out := serverInfoOut{
			Name:     "argus",
			Version:  Version,
			ReadOnly: true, // enforced by TestNoMutatingVerbs, not by convention
			Tools:    []string{"server_info", "diagnose_workload", "get_workload_logs"},
		}
		return nil, out, nil
	})
}
