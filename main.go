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
//	argus logs foo -n ns           # logs for the failing container, with judgment
//	argus triage                   # what is broken right now, cluster-wide
//	argus pending foo -n ns        # why a workload will not schedule
//	argus update                   # replace this binary with the latest release
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"errors"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sigs.k8s.io/yaml"

	"github.com/backendArchitect/argus/internal/detect"
	"github.com/backendArchitect/argus/internal/kube"
	"github.com/backendArchitect/argus/internal/selfupdate"
	"github.com/backendArchitect/argus/internal/tools"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "argus:", err)
		os.Exit(1)
	}
}

// commands is the dispatch table, and also what `argus help` prints. One source so the two cannot
// disagree — a help text that lists a command the binary does not have is worse than no help.
var commands = []struct {
	name, args, blurb string
	run               func(context.Context, []string, io.Writer) error
}{
	{"diagnose", "<workload> -n <ns>", "diagnose a workload: ranked causes, each citing evidence", diagnose},
	{"logs", "<workload> -n <ns>", "logs for the failing container, with judgment", logs},
	{"triage", "[-n <ns>]", "what is broken right now, grouped by controller", triage},
	{"pending", "<workload> -n <ns>", "why a workload's pods will not schedule", pending},
	{"serve", "", "run as an MCP server over stdio", serve},
	{"capture", "<workload> -n <ns>", "write a diagnosis snapshot as YAML (the fixture generator)", capture},
	{"version", "", "print the version", cmdVersion},
	{"update", "", "replace this binary with the latest release", cmdUpdate},
}

func run(args []string, stdout io.Writer) error {
	// Help must work before anything else and must exit 0. Previously `argus --help` fell through
	// to the serve flagset, printed "Usage of serve:", and exited 1 with "flag: help requested" —
	// which reads as a crash rather than as help.
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return nil
	}
	if args[0] == "-v" || args[0] == "--version" {
		return cmdVersion(context.Background(), nil, stdout)
	}

	// Interrupt cancels in-flight gathers so we never leave apiserver calls hanging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, c := range commands {
		if c.name == args[0] {
			err := c.run(ctx, args[1:], stdout)
			// flag already printed the command's usage; -h is not a failure.
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
	}
	return fmt.Errorf("unknown command %q — run 'argus help' for the list", args[0])
}

// cmdUpdate replaces the running binary. The checksum verification and the refusal to clobber a
// local build live in internal/selfupdate; this is only the flag surface.
// pending explains unschedulability, node by node.
func pending(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("pending", flag.ContinueOnError)
	ns := fs.String("n", "", "namespace (required)")
	fs.StringVar(ns, "namespace", "", "namespace (required)")
	opts := clusterFlags(fs)
	describe(fs, "why a workload's pods will not schedule",
		"argus pending checkout-api -n prod")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return fmt.Errorf("pending needs exactly one workload name")
	}

	c, err := kube.New(*opts)
	if err != nil {
		return err
	}
	ref, err := c.Resolve(ctx, positional[0], *ns)
	if err != nil {
		return err
	}
	reports, err := c.Pending(ctx, ref)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, detect.RenderPending(reports))
	fmt.Fprintf(os.Stderr, "\n(%d apiserver calls against %s)\n", c.Calls(), c.Context)
	return nil
}

// triage scans a namespace or the whole cluster.
func triage(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	ns := fs.String("n", "", "namespace (default: every namespace)")
	fs.StringVar(ns, "namespace", "", "namespace (default: every namespace)")
	opts := clusterFlags(fs)
	describe(fs, "what is broken right now, grouped by controller",
		"argus triage            # or: argus triage -n prod")
	if _, err := parseInterleaved(fs, args); err != nil {
		return err
	}

	c, err := kube.New(*opts)
	if err != nil {
		return err
	}
	snaps, degraded, notes, err := c.Triage(ctx, *ns)
	if err != nil {
		return err
	}
	scope := "cluster"
	if *ns != "" {
		scope = "namespace/" + *ns
	}
	fmt.Fprint(stdout, detect.RenderTriage(detect.Triage(scope, snaps, degraded, notes)))
	fmt.Fprintf(os.Stderr, "\n(%d apiserver calls against %s)\n", c.Calls(), c.Context)
	return nil
}

func cmdUpdate(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	force := fs.Bool("force", false, "replace even a locally-built binary (this discards your local build)")
	check := fs.Bool("check", false, "report whether an update exists, change nothing")
	describe(fs, "replace this binary with the latest release",
		"argus update            # or: argus update -check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return selfupdate.Update(ctx, selfupdate.Options{
		Current: tools.Version, Force: *force, DryRun: *check,
	}, stdout)
}

func cmdVersion(_ context.Context, _ []string, stdout io.Writer) error {
	fmt.Fprintln(stdout, tools.Version)
	return nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, `argus — read-only Kubernetes incident diagnosis

One question in, one ranked diagnosis out. argus never writes to your cluster.

Usage:
  argus <command> [flags]

Commands:
`)
	for _, c := range commands {
		fmt.Fprintf(w, "  %-9s %-20s %s\n", c.name, c.args, c.blurb)
	}
	fmt.Fprint(w, `
Cluster flags, accepted by every command that reads a cluster:
  -n, -namespace string   namespace (required)
  -kubeconfig string      path to kubeconfig (default: standard rules, then in-cluster)
  -context string         kubeconfig context (default: current-context)
  -timeout duration       deadline for the whole gather (default 10s)
  -max-calls int          hard cap on apiserver requests per invocation (default 60)

The workload name is fuzzy: "checkout", "checkout-api" and "deploy/checkout-api"
all resolve, and an ambiguous name lists the candidates instead of guessing.

Examples:
  argus diagnose checkout-api -n prod
  argus logs checkout-api -n prod
  argus capture deploy/checkout-api -n prod > snapshot.yaml
  claude mcp add argus -- argus serve

Run 'argus <command> -h' for one command's flags.
Docs: https://github.com/backendArchitect/argus
`)
}

// describe gives a subcommand's flagset a real usage block instead of the bare
// "Usage of diagnose:" that flag prints by default.
func describe(fs *flag.FlagSet, blurb, example string) {
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "argus %s — %s\n\nUsage:\n  %s\n\nFlags:\n", fs.Name(), blurb, example)
		fs.PrintDefaults()
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
func capture(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	ns := fs.String("n", "", "namespace (required)")
	fs.StringVar(ns, "namespace", "", "namespace (required)")
	opts := clusterFlags(fs)
	describe(fs, "write a diagnosis snapshot as YAML (the fixture generator)",
		"argus capture deploy/checkout-api -n prod > snapshot.yaml")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return fmt.Errorf("capture needs exactly one workload name")
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
	_, err = stdout.Write(out)
	return err
}

func serve(ctx context.Context, args []string, _ io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	opts := clusterFlags(fs)
	describe(fs, "run as an MCP server over stdio",
		"argus serve      # then: claude mcp add argus -- argus serve")
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
func diagnose(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	ns := fs.String("n", "", "namespace (required)")
	fs.StringVar(ns, "namespace", "", "namespace (required)")
	opts := clusterFlags(fs)
	describe(fs, "diagnose a workload: ranked causes, each citing evidence",
		"argus diagnose checkout-api -n prod")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return fmt.Errorf("diagnose needs exactly one workload name")
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

	fmt.Fprint(stdout, detect.Render(snap, detect.All(snap)))
	fmt.Fprintf(os.Stderr, "\n(%d apiserver calls against %s)\n", c.Calls(), c.Context)
	return nil
}

// logs fetches container output with the same selection logic the MCP tool uses.
func logs(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	ns := fs.String("n", "", "namespace (required)")
	fs.StringVar(ns, "namespace", "", "namespace (required)")
	container := fs.String("container", "", "container name (default: the failing one, skipping sidecars)")
	prev := fs.Bool("previous", false, "read the previous container instance")
	prevSet := false
	since := fs.Int64("since", 0, "only logs newer than this many seconds")
	tail := fs.Int64("tail", 0, "max lines to request from the apiserver")
	opts := clusterFlags(fs)
	describe(fs, "logs for the failing container, with judgment",
		"argus logs checkout-api -n prod")
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "previous" {
			prevSet = true
		}
	})
	if len(positional) != 1 {
		fs.Usage()
		return fmt.Errorf("logs needs exactly one workload name")
	}

	c, err := kube.New(*opts)
	if err != nil {
		return err
	}
	ref, err := c.Resolve(ctx, positional[0], *ns)
	if err != nil {
		return err
	}

	// Only pass Previous through when the user actually set it. Its absence is what lets argus
	// decide, which is the behaviour worth having.
	lo := kube.LogOptions{Container: *container, SinceSecs: *since, TailLines: *tail}
	if prevSet {
		lo.Previous = prev
	}

	b, err := c.Logs(ctx, ref, lo)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, kube.RenderLogs(b))
	fmt.Fprintf(os.Stderr, "\n(%d apiserver calls against %s)\n", c.Calls(), c.Context)
	return nil
}
