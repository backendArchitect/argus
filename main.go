// Command argus is a read-only Kubernetes incident-diagnosis MCP server.
//
// It answers SRE questions ("why is this workload broken?") with a ranked, evidence-backed
// diagnosis rather than exposing Kubernetes resources for a model to correlate itself.
//
// Usage:
//
//	argus serve                    # MCP server over stdio (default)
//	argus capture deploy/foo -n ns # write a Snapshot fixture to stdout
//	argus diagnose foo -n ns       # run the pipeline from the CLI, no MCP
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/yaml"

	"github.com/backendArchitect/argus/internal/detect"
	"github.com/backendArchitect/argus/internal/kube"
	"github.com/backendArchitect/argus/internal/tools"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "argus:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	// Interrupt cancels in-flight gathers so we never leave apiserver calls hanging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return serve(ctx, args)
	case "capture":
		return capture(ctx, args)
	case "diagnose":
		return diagnose(ctx, args)
	case "version":
		fmt.Println(tools.Version)
		return nil
	default:
		return fmt.Errorf("unknown command %q (want: serve, diagnose, capture, version)", cmd)
	}
}

// parseInterleaved parses flags that appear before, after, or between positional arguments.
//
// flag.Parse stops at the first non-flag argument, so "argus capture foo -n prod" would silently
// ignore -n and diagnose the wrong namespace. Everyone types it that way because kubectl accepts
// it, and silently reading the wrong namespace during an incident is not an acceptable failure.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// clusterFlags registers the flags every cluster-touching subcommand shares.
func clusterFlags(fs *flag.FlagSet) *kube.Options {
	o := kube.DefaultOptions()
	fs.StringVar(&o.Kubeconfig, "kubeconfig", "", "path to kubeconfig (default: standard rules, then in-cluster)")
	fs.StringVar(&o.Context, "context", "", "kubeconfig context (default: current-context)")
	fs.DurationVar(&o.Timeout, "timeout", o.Timeout, "per-invocation deadline for the whole gather")
	fs.Int64Var(&o.MaxCalls, "max-calls", o.MaxCalls, "hard cap on apiserver requests per invocation")
	return &o
}

// capture writes a Snapshot to stdout as YAML.
//
// This is the fixture generator: capture a real broken workload, commit the YAML under
// testdata/snapshots/, and the detector tests replay it forever with no cluster. Production and
// tests therefore run the identical gather -> project -> detect path.
func capture(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	ns := fs.String("n", "", "namespace")
	fs.StringVar(ns, "namespace", "", "namespace")
	opts := clusterFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: argus capture <workload> -n <namespace>")
	}

	c, err := kube.New(*opts)
	if err != nil {
		return err
	}
	ref, err := c.Resolve(ctx, positional[0], *ns)
	if err != nil {
		return err
	}
	snap, err := c.Gather(ctx, ref)
	if err != nil {
		return err
	}

	out, err := yaml.Marshal(snap)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "captured %s from %s in %d apiserver calls", ref, c.Context, c.Calls())
	if len(snap.Degraded) > 0 {
		fmt.Fprintf(os.Stderr, " (degraded: %d)", len(snap.Degraded))
	}
	fmt.Fprintln(os.Stderr)
	_, err = os.Stdout.Write(out)
	return err
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	opts := clusterFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// stdio carries the JSON-RPC framing, so every diagnostic must go to stderr.
	fmt.Fprintf(os.Stderr, "argus %s serving on stdio (read-only)\n", tools.Version)
	return tools.Server(*opts).Run(ctx, &mcp.StdioTransport{})
}

// diagnose runs the full pipeline from the CLI, with no MCP in the way.
//
// Useful for dogfooding and for CI: the same gather -> detect -> rank path the
// MCP tool uses, so a difference between them would be a bug rather than a
// second implementation drifting.
func diagnose(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	ns := fs.String("n", "", "namespace")
	fs.StringVar(ns, "namespace", "", "namespace")
	opts := clusterFlags(fs)
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: argus diagnose <workload> -n <namespace>")
	}

	c, err := kube.New(*opts)
	if err != nil {
		return err
	}
	ref, err := c.Resolve(ctx, positional[0], *ns)
	if err != nil {
		return err
	}
	snap, err := c.Gather(ctx, ref)
	if err != nil {
		return err
	}

	fmt.Print(detect.Render(snap, detect.All(snap)))
	fmt.Fprintf(os.Stderr, "\n(%d apiserver calls against %s)\n", c.Calls(), c.Context)
	return nil
}
