package tools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/backendArchitect/argus/internal/kube"
)

// connect wires a client to Server() over an in-memory transport, exercising the real handshake
// without a subprocess.
func connect(t *testing.T) (*mcp.ClientSession, context.Context) {
	t.Helper()
	ctx := t.Context()
	clientT, serverT := mcp.NewInMemoryTransports()

	// Zero Options: the server must build without a kubeconfig, and server_info must
	// answer regardless. Only diagnose_workload needs a cluster, and only when called.
	srv := Server(kube.Options{})
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, ctx
}

// TestToolSurface pins the tool count. The design rule is one tool per SRE question — above ~12 we
// have started mirroring kubectl, which is the thing argus exists not to be.
func TestToolSurface(t *testing.T) {
	cs, ctx := connect(t)

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("no tools registered")
	}
	if len(res.Tools) > 12 {
		t.Errorf("tool surface grew to %d; one tool per SRE question, not per resource", len(res.Tools))
	}
	for _, tool := range res.Tools {
		if tool.Description == "" {
			t.Errorf("tool %q has no description; the model selects on it", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
	}
}

// TestServerInfo is the handshake canary: it needs no cluster, so a failure here is transport.
func TestServerInfo(t *testing.T) {
	cs, ctx := connect(t)

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "server_info"})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Fatalf("server_info returned an error result: %+v", res.Content)
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected structured content, got %T", res.StructuredContent)
	}
	if sc["read_only"] != true {
		t.Errorf("read_only = %v, want true", sc["read_only"])
	}
	if sc["name"] != "argus" {
		t.Errorf("name = %v, want argus", sc["name"])
	}
}
