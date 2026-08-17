package project

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestLogsCollapseRepetition is the whole reason this layer exists. A crashlooping service emits
// the same failure thousands of times; a diagnosis that pastes all of them into context has spent
// the window before reaching the panic that explains it.
func TestLogsCollapseRepetition(t *testing.T) {
	var b strings.Builder
	b.WriteString("starting up\n")
	// Each line differs by IP, so this only collapses if normalization runs before grouping.
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "dial tcp 10.4.2.%d:5432: connect: connection refused\n", i%250)
	}
	b.WriteString("panic: runtime error: invalid memory address\n")

	groups, dropped := Logs(b.String(), time.Now(), DefaultLogTokens)
	if dropped != 0 {
		t.Errorf("dropped %d groups, expected everything to fit after collapsing", dropped)
	}
	if len(groups) != 3 {
		t.Fatalf("202 lines collapsed to %d groups, want 3:\n%+v", len(groups), groups)
	}

	var found bool
	for _, g := range groups {
		if strings.Contains(g.Text, "connection refused") {
			found = true
			if g.Count != 200 {
				t.Errorf("count = %d, want 200 — collapsing must not lose the repetition count", g.Count)
			}
		}
	}
	if !found {
		t.Error("the repeated line vanished entirely")
	}
	// The panic must survive: it is the only line that explains anything.
	if !strings.Contains(groups[len(groups)-1].Text, "panic") {
		t.Errorf("last group is %q, want the panic", groups[len(groups)-1].Text)
	}
}

// TestLogsKeepTheTail pins which end of the log the budget sacrifices. A failure appears at the end
// of a log; keeping the first N lines would reliably discard the only useful part.
func TestLogsKeepTheTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "line %d some reasonably long padding text to consume the budget\n", i)
	}
	b.WriteString("panic: the thing that actually matters\n")

	groups, dropped := Logs(b.String(), time.Now(), 200)
	if dropped == 0 {
		t.Fatal("expected the budget to drop something")
	}
	last := groups[len(groups)-1].Text
	if !strings.Contains(last, "panic") {
		t.Errorf("last kept line is %q; the tail must survive the budget", last)
	}
}

// TestLogsRedactBeforeGrouping guards the ordering. If grouping ran first, a credential would
// already be inside a group key and a count, and redaction afterwards could not reach it.
func TestLogsRedactBeforeGrouping(t *testing.T) {
	raw := "boot: DATABASE_URL=postgres://svc:hunter2CorrectHorse@db:5432/app\n" +
		"boot: DATABASE_URL=postgres://svc:hunter2CorrectHorse@db:5432/app\n"
	groups, _ := Logs(raw, time.Now(), DefaultLogTokens)

	for _, g := range groups {
		if strings.Contains(g.Text, "hunter2CorrectHorse") {
			t.Fatalf("credential survived into a log group: %s", g.Text)
		}
	}
	if len(groups) != 1 || groups[0].Count != 2 {
		t.Errorf("got %d group(s) with count %d, want 1 with count 2", len(groups), groups[0].Count)
	}
}

// TestLogsParseTimestamps covers the RFC3339 prefix the kubelet adds, and the common case of a
// container that emits none.
func TestLogsParseTimestamps(t *testing.T) {
	now := time.Now()
	stamped := now.Add(-90*time.Second).UTC().Format(time.RFC3339Nano) + " ready to serve\n"
	groups, _ := Logs(stamped, now, DefaultLogTokens)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groups[0].Text != "ready to serve" {
		t.Errorf("text = %q, want the timestamp stripped", groups[0].Text)
	}
	if groups[0].LastSecondsAgo < 89 || groups[0].LastSecondsAgo > 91 {
		t.Errorf("age = %ds, want ~90", groups[0].LastSecondsAgo)
	}

	plain, _ := Logs("no timestamp here\n", now, DefaultLogTokens)
	if len(plain) != 1 || plain[0].Text != "no timestamp here" {
		t.Errorf("unstamped line mangled: %+v", plain)
	}
}

func TestLogsIgnoreBlankLines(t *testing.T) {
	groups, _ := Logs("a\n\n\n   \nb\n", time.Now(), DefaultLogTokens)
	if len(groups) != 2 {
		t.Errorf("got %d groups, want 2 (blank lines dropped): %+v", len(groups), groups)
	}
}
