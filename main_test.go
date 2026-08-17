package main

import (
	"bytes"
	"flag"
	"io"
	"strings"
	"testing"
)

// TestHelpExitsZero pins the behaviour that made argus look broken: `argus --help` used to fall
// through to the serve flagset, print "Usage of serve:", and exit 1 with "flag: help requested"
// leaking out. Asking for help is not an error.
func TestHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{{}, {"help"}, {"-h"}, {"--help"}} {
		var out bytes.Buffer
		if err := run(args, &out); err != nil {
			t.Errorf("run(%v) returned %v, want nil — help is not a failure", args, err)
		}
		got := out.String()
		if !strings.Contains(got, "argus — read-only Kubernetes incident diagnosis") {
			t.Errorf("run(%v) did not print the top-level help:\n%s", args, got)
		}
		if strings.Contains(got, "Usage of serve") {
			t.Errorf("run(%v) printed the serve flagset instead of the top-level help", args)
		}
	}
}

// TestHelpListsEveryCommand is why the dispatch table and the help text share one source. A help
// text that drifts from the binary is worse than none, because it is confidently wrong.
func TestHelpListsEveryCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"help"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range commands {
		if !strings.Contains(out.String(), c.name) {
			t.Errorf("help does not mention the %q command", c.name)
		}
		if c.blurb == "" {
			t.Errorf("command %q has no blurb, so help would list it without saying what it does", c.name)
		}
		if c.run == nil {
			t.Errorf("command %q has no implementation", c.name)
		}
	}
}

func TestVersionFlags(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"-v"}, {"--version"}} {
		var out bytes.Buffer
		if err := run(args, &out); err != nil {
			t.Errorf("run(%v) = %v, want nil", args, err)
		}
		if strings.TrimSpace(out.String()) == "" {
			t.Errorf("run(%v) printed no version", args)
		}
	}
}

// TestUnknownCommandIsAnError guards the other direction: a typo must fail loudly and point at
// help, not silently start a server and appear to hang.
func TestUnknownCommandIsAnError(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"diagnoze"}, &out)
	if err == nil {
		t.Fatal("a misspelled command must be an error")
	}
	if !strings.Contains(err.Error(), "argus help") {
		t.Errorf("error %q should point the reader at 'argus help'", err)
	}
}

// TestSubcommandHelpExitsZero covers `argus <cmd> -h`. flag returns ErrHelp after printing usage,
// and treating that as a failure would make every help request look like a crash.
func TestSubcommandHelpExitsZero(t *testing.T) {
	for _, name := range []string{"diagnose", "logs", "capture", "serve"} {
		var out bytes.Buffer
		if err := run([]string{name, "-h"}, &out); err != nil {
			t.Errorf("run(%s -h) = %v, want nil", name, err)
		}
	}
}

// TestMissingWorkloadNameFails checks the usage error rather than a nil-pointer or a cluster call.
// It must not need a kubeconfig to reach this point.
func TestMissingWorkloadNameFails(t *testing.T) {
	for _, name := range []string{"diagnose", "logs", "capture"} {
		var out bytes.Buffer
		err := run([]string{name}, &out)
		if err == nil {
			t.Errorf("run(%s) with no workload should fail", name)
			continue
		}
		if !strings.Contains(err.Error(), "exactly one workload") {
			t.Errorf("run(%s) error = %q, want it to name the missing argument", name, err)
		}
	}
}

// TestParseInterleaved covers the kubectl-style ordering everyone types. flag.Parse stops at the
// first positional, so without this `argus diagnose foo -n prod` silently reads the wrong
// namespace — a quiet wrong answer during an incident.
func TestParseInterleaved(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"flags after positional", []string{"checkout", "-n", "prod"}},
		{"flags before positional", []string{"-n", "prod", "checkout"}},
		{"flags either side", []string{"-timeout", "5s", "checkout", "-n", "prod"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			ns := fs.String("n", "", "namespace")
			fs.Duration("timeout", 0, "")
			positional, err := parseInterleaved(fs, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if len(positional) != 1 || positional[0] != "checkout" {
				t.Errorf("positional = %v, want [checkout]", positional)
			}
			if *ns != "prod" {
				t.Errorf("namespace = %q, want prod — the flag was dropped", *ns)
			}
		})
	}
}
