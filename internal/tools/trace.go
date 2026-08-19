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

type traceIn struct {
	Service   string `json:"service" jsonschema:"Service name. Fuzzy: 'checkout' or 'svc/checkout-api' both work."`
	Namespace string `json:"namespace" jsonschema:"Namespace the Service is in. Required."`
}

type traceOut struct {
	Report *model.TraceReport `json:"report"`
	Calls  int64              `json:"apiserver_calls"`
}

func registerTrace(s *mcp.Server, opts kube.Options) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "trace_service_path",
		Description: "Trace the request path to a Service and report the first hop that breaks: " +
			"Ingress rule, selector, targetPort resolution, endpoint addresses, endpoint " +
			"readiness. Answers 'the Service exists and the pods are ready, so why does traffic " +
			"not arrive?'. Reports only the FIRST break, since everything downstream of it is " +
			"unreachable rather than unhealthy, and names the causes it cannot see — " +
			"NetworkPolicy, mesh sidecars, whether the process is really listening — because an " +
			"intact chain means the answer is one of those.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in traceIn) (*mcp.CallToolResult, traceOut, error) {
		if in.Service == "" || in.Namespace == "" {
			return nil, traceOut{}, errors.New("both service and namespace are required")
		}
		c, err := kube.New(opts)
		if err != nil {
			return nil, traceOut{}, fmt.Errorf("cluster access: %w", err)
		}
		r, err := c.Trace(ctx, in.Service, in.Namespace)
		if err != nil {
			var amb *kube.AmbiguousError
			if errors.As(err, &amb) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: amb.Error()}}},
					traceOut{}, nil
			}
			return nil, traceOut{}, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: detect.RenderTrace(r),
		}}}, traceOut{Report: r, Calls: c.Calls()}, nil
	})
}
