package project

import (
	"strings"
	"time"

	"github.com/backendArchitect/argus/internal/model"
)

// DefaultLogTokens is the ceiling on emitted log content.
//
// Budgeting by tokens rather than lines is the point: "last 100 lines" is meaningless when one
// line is a 4KB JSON blob and the next is "ok". A model's context is spent in tokens, so that is
// the unit the limit has to be expressed in.
const DefaultLogTokens = 1800

// Logs turns raw container output into redacted, grouped, budgeted lines.
//
// Order of operations matters and is deliberate:
//
//  1. redact first, so a credential can never survive into a group key or a count;
//  2. group second, so repetition collapses before the budget is spent on it;
//  3. budget last, keeping the NEWEST groups — a crash is at the end of the log, never the start.
func Logs(raw string, now time.Time, tokenBudget int) ([]model.LogGroup, int) {
	if tokenBudget <= 0 {
		tokenBudget = DefaultLogTokens
	}

	type acc struct {
		g     *model.LogGroup
		order int
	}
	groups := map[string]*acc{}
	var order []string

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		ts, text := splitTimestamp(line)
		text = RedactLine(text)
		if strings.TrimSpace(text) == "" {
			continue
		}

		// Group on the normalized form — the same normalization used for events, which already
		// strips UIDs, IPs, digests, durations and generated names. Two panics differing only by
		// a goroutine id are the same panic.
		key := Normalize(text)
		age := int64(0)
		if !ts.IsZero() {
			age = secondsAgo(now, ts)
		}

		if a, ok := groups[key]; ok {
			a.g.Count++
			if !ts.IsZero() {
				if age < a.g.LastSecondsAgo || a.g.LastSecondsAgo == 0 {
					a.g.LastSecondsAgo = age
				}
				if age > a.g.FirstSecondsAgo {
					a.g.FirstSecondsAgo = age
				}
			}
			continue
		}
		// Keep the first raw form as the representative rather than the normalized one: the
		// reader wants the actual message, placeholders only served the grouping.
		groups[key] = &acc{g: &model.LogGroup{
			Text: text, Count: 1, FirstSecondsAgo: age, LastSecondsAgo: age,
		}, order: len(order)}
		order = append(order, key)
	}

	// Spend the budget from the newest end backwards, then restore chronological order.
	var kept []model.LogGroup
	spent, dropped := 0, 0
	for i := len(order) - 1; i >= 0; i-- {
		g := *groups[order[i]].g
		cost := len(g.Text)/4 + 1
		if spent+cost > tokenBudget && len(kept) > 0 {
			dropped = i + 1
			break
		}
		spent += cost
		kept = append(kept, g)
	}
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}
	return kept, dropped
}

// splitTimestamp peels the RFC3339 prefix the kubelet adds when Timestamps is requested. A line
// without one is not an error — many runtimes and sidecars emit raw text.
func splitTimestamp(line string) (time.Time, string) {
	sp := strings.IndexByte(line, ' ')
	if sp <= 0 {
		return time.Time{}, line
	}
	ts, err := time.Parse(time.RFC3339Nano, line[:sp])
	if err != nil {
		return time.Time{}, line
	}
	return ts, line[sp+1:]
}
