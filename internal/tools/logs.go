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

type logsIn struct {
	Workload  string `json:"workload" jsonschema:"Workload name. Fuzzy: 'checkout' or 'checkout-api' both work, and it may be kind-qualified as deploy/checkout-api."`
	Namespace string `json:"namespace" jsonschema:"Namespace to look in. Required."`
	Container string `json:"container,omitempty" jsonschema:"Container name. Omit to let argus pick the failing one rather than a sidecar."`
	Previous  *bool  `json:"previous,omitempty" jsonschema:"Read the previous container instance. Omit to decide automatically - on a crashlooping container this defaults to true, because the current instance is in backoff and has produced nothing."`
	SinceSecs int64  `json:"since_seconds,omitempty" jsonschema:"Only return logs newer than this many seconds. Omit for no lower bound."`
	TailLines int64  `json:"tail_lines,omitempty" jsonschema:"Maximum lines to request from the apiserver. Output is separately capped by a token budget."`
}

type logsOut struct {
	Pod       string           `json:"pod"`
	Container string           `json:"container"`
	Previous  bool             `json:"previous" jsonschema:"whether these are the previous (died) instance's logs"`
	Reason    string           `json:"reason" jsonschema:"why this pod, container and instance were chosen"`
	Groups    []model.LogGroup `json:"groups" jsonschema:"lines identical after normalization are collapsed with a count"`
	Dropped   int              `json:"dropped_groups,omitempty"`
	Note      string           `json:"note,omitempty"`
	Calls     int64            `json:"apiserver_calls"`
}

func registerLogs(s *mcp.Server, opts kube.Options) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_workload_logs",
		Description: "Read container logs for a workload, with judgment. Picks the failing pod and " +
			"container rather than a sidecar, and on a crashlooping container reads the PREVIOUS " +
			"instance by default because the current one is in backoff and has written nothing. " +
			"Repeated lines are collapsed with counts, credentials are redacted, and output is " +
			"capped by a token budget. Prefer this over fetching logs per-pod yourself.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in logsIn) (*mcp.CallToolResult, logsOut, error) {
		if in.Workload == "" || in.Namespace == "" {
			return nil, logsOut{}, errors.New("both workload and namespace are required")
		}
		c, err := kube.New(opts)
		if err != nil {
			return nil, logsOut{}, fmt.Errorf("cluster access: %w", err)
		}
		ref, err := c.Resolve(ctx, in.Workload, in.Namespace)
		if err != nil {
			var amb *kube.AmbiguousError
			if errors.As(err, &amb) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: amb.Error()}}},
					logsOut{}, nil
			}
			return nil, logsOut{}, err
		}

		b, err := c.Logs(ctx, ref, kube.LogOptions{
			Container: in.Container, Previous: in.Previous,
			TailLines: in.TailLines, SinceSecs: in.SinceSecs,
		})
		if err != nil {
			return nil, logsOut{}, err
		}

		out := logsOut{
			Pod: b.Pod, Container: b.Container, Previous: b.Previous, Reason: b.Reason,
			Groups: b.Groups, Dropped: b.DroppedGroups, Note: b.Note, Calls: c.Calls(),
		}
		// Logs are the highest-risk injection vector argus emits: every byte is application-
		// controlled. The framing says so; the absence of any mutating tool is what makes it safe.
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
			Text: detect.UntrustedNote + "\n" + kube.RenderLogs(b),
		}}}, out, nil
	})
}
