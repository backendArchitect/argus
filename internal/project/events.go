// Package project reduces raw Kubernetes objects to the allowlisted fields argus emits.
//
// Raw objects are grotesque: a single Pod carries managedFields, a full duplicate of its own spec
// under last-applied-configuration, resourceVersion, uid, and generated token mounts. Emitting
// them fills the model's context with noise, and a model reasoning from noise guesses.
//
// Every projection here is an explicit allowlist. Never a blocklist — a blocklist silently
// regresses the moment Kubernetes adds a field.
package project

import (
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/backendArchitect/argus/internal/model"
)

// noise are the varying substrings that make otherwise-identical event messages look distinct.
// Stripping them is what collapses a hundred BackOff events into one line without losing anything.
var noise = []struct {
	re   *regexp.Regexp
	with string
}{
	// UUIDs, then image digests, then IPs, then timestamps, then generated name suffixes.
	{regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<uid>"},
	// No trailing \b: a digest runs straight into the closing quote, and pinning the length to
	// exactly 64 would silently stop matching on any registry that formats them differently.
	{regexp.MustCompile(`\bsha256:[0-9a-f]{7,}`), "<digest>"},
	{regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`), "<ip>"},
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?\b`), "<time>"},
	// Pod name suffixes: "-7d9f8b6c5d-x2k9p" (replicaset hash + pod suffix) and bare "-x2k9p".
	{regexp.MustCompile(`-[0-9a-f]{8,10}-[a-z0-9]{5}\b`), "-<pod>"},
	{regexp.MustCompile(`\b[0-9a-f]{7,40}\b`), "<hash>"},
	// Container IDs and durations that tick on every retry.
	{regexp.MustCompile(`\bcontainerd://\S+`), "<containerid>"},
	{regexp.MustCompile(`\b\d+(\.\d+)?(ns|us|ms|s|m|h)\b`), "<dur>"},
}

// Normalize strips the varying parts of an event message so equivalent events group together.
func Normalize(msg string) string {
	out := strings.TrimSpace(msg)
	for _, n := range noise {
		out = n.re.ReplaceAllString(out, n.with)
	}
	return strings.Join(strings.Fields(out), " ")
}

// Events deduplicates raw events into groups keyed by (type, reason, normalized message, kind).
//
// The object name is deliberately NOT part of the key. Forty pods of one Deployment reporting the
// same BackOff is one fact about the Deployment, not forty facts — keying on the object would
// produce forty near-identical groups and defeat the entire purpose. Each group instead reports
// how many distinct objects it covers, plus one of them as an example to drill into.
//
// Counts respect the event's own series/count field rather than being recomputed: the apiserver
// already aggregates repeats server-side, so counting occurrences here would undercount by orders
// of magnitude on exactly the events that matter most.
func Events(evs []corev1.Event, now time.Time) []model.EventGroup {
	type key struct{ typ, reason, msg, kind string }
	groups := map[key]*model.EventGroup{}
	objects := map[key]map[string]bool{}

	for i := range evs {
		e := &evs[i]
		k := key{e.Type, e.Reason, Normalize(e.Message), e.InvolvedObject.Kind}

		count := e.Count
		if count == 0 {
			count = 1 // events.k8s.io writes series-based events with Count unset
		}
		if e.Series != nil && e.Series.Count > count {
			count = e.Series.Count
		}

		if objects[k] == nil {
			objects[k] = map[string]bool{}
		}
		objects[k][e.InvolvedObject.Name] = true

		first, last := eventWindow(e)
		g, ok := groups[k]
		if !ok {
			groups[k] = &model.EventGroup{
				Type: e.Type, Reason: e.Reason, Message: k.msg, Count: count,
				ObjectKind: e.InvolvedObject.Kind, ObjectName: e.InvolvedObject.Name,
				FirstSeenSecondsAgo: secondsAgo(now, first),
				LastSeenSecondsAgo:  secondsAgo(now, last),
			}
			continue
		}
		g.Count += count
		// Keep the lexically first name as the example so the output is deterministic across runs;
		// map iteration order would otherwise make committed fixtures flap.
		if e.InvolvedObject.Name < g.ObjectName {
			g.ObjectName = e.InvolvedObject.Name
		}
		if a := secondsAgo(now, first); a > g.FirstSeenSecondsAgo {
			g.FirstSeenSecondsAgo = a
		}
		if a := secondsAgo(now, last); a < g.LastSeenSecondsAgo {
			g.LastSeenSecondsAgo = a
		}
	}

	out := make([]model.EventGroup, 0, len(groups))
	for k, g := range groups {
		g.ObjectCount = len(objects[k])
		out = append(out, *g)
	}
	// Warnings first, then most recent — the order an SRE reads them in.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == corev1.EventTypeWarning) != (out[j].Type == corev1.EventTypeWarning) {
			return out[i].Type == corev1.EventTypeWarning
		}
		if out[i].LastSeenSecondsAgo != out[j].LastSeenSecondsAgo {
			return out[i].LastSeenSecondsAgo < out[j].LastSeenSecondsAgo
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// eventWindow picks the best available timestamps. Events carry up to three different time fields
// depending on which API version wrote them, and older clusters leave some of them zero.
func eventWindow(e *corev1.Event) (first, last time.Time) {
	first = e.FirstTimestamp.Time
	last = e.LastTimestamp.Time
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		last = e.Series.LastObservedTime.Time
	}
	if first.IsZero() {
		first = e.EventTime.Time
	}
	if last.IsZero() {
		last = first
	}
	if first.IsZero() {
		first, last = e.CreationTimestamp.Time, e.CreationTimestamp.Time
	}
	return first, last
}

// secondsAgo converts an absolute time to an age. Snapshots store ages so that committed fixtures
// never rot — see model.Snapshot.
func secondsAgo(now, t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	d := int64(now.Sub(t).Seconds())
	if d < 0 {
		return 0 // clock skew between the apiserver and here
	}
	return d
}
