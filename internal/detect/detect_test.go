package detect

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/backendArchitect/argus/internal/model"
)

// fixtures maps a captured snapshot to the detector IDs it must produce.
//
// The second half of each expectation — that NOTHING ELSE fires — is the half
// that matters. A detector suite is easy to make eager and hard to make
// discriminating, and a tool that reports three wrong causes during an incident
// costs more time than it saves. `healthy` is the strongest test here precisely
// because it expects nothing at all.
var fixtures = map[string][]string{
	"oom-limit-too-low": {"oomkill.limit-too-low"},
	"image-pull-typo":   {"image.pull-not-found"},
	// The same failure hours later, after Kubernetes expired the event that named the cause.
	"image-pull-expired": {"image.pull-failed"},
	"readiness-too-fast": {"probe.readiness-misconfigured", "endpoints.no-ready-backends"},
	"endpoint-gap":       {"endpoints.selector-matches-nothing"},
	"bad-rollout":        {"rollout.bad-template", "oomkill.limit-too-low"},
	"healthy":            {},
	"node-pressure":      {"node.unhealthy-host"},
	// The most common Kubernetes failure of all, and argus was silent on it until now.
	"crashloop-nonzero":    {"crashloop.exiting-nonzero"},
	"crashloop-wont-start": {"crashloop.container-wont-start"},
	// Regressions, each distilled from a real false positive found by running against a live
	// cluster. All three assert SILENCE, which is the hardest property to keep true as detectors
	// grow — and the one that decides whether anyone trusts the tool at 3am.
	"scaled-to-zero":         {}, // idle workload at 0/0 reported five criticals
	"healthy-after-rollback": {}, // healthy 1/1 reported as a failing rollout
	"old-oomkill-recovered":  {}, // OOMKill 18 days ago on a recovered pod, reported critical
}

func load(t *testing.T, name string) *model.Snapshot {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "snapshots", name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("fixture %s missing (run hack/rebuild-fixtures.sh): %v", name, err)
	}
	var s model.Snapshot
	if err := yaml.Unmarshal(data, &s); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return &s
}

// TestFixtures is the moat: every fixture must produce exactly its expected
// detectors, no more and no fewer.
func TestFixtures(t *testing.T) {
	for name, want := range fixtures {
		t.Run(name, func(t *testing.T) {
			snap := load(t, name)
			findings := All(snap)

			var got []string
			for _, f := range findings {
				got = append(got, f.ID)
			}
			sort.Strings(got)
			wantSorted := append([]string(nil), want...)
			sort.Strings(wantSorted)

			if strings.Join(got, ",") != strings.Join(wantSorted, ",") {
				t.Errorf("detectors fired: %v\n            want: %v", got, wantSorted)
				for _, f := range findings {
					t.Logf("  [%s] %.2f %s — %s", f.Severity, f.Confidence, f.ID, f.Title)
				}
			}
		})
	}
}

// TestEveryFindingCitesEvidence enforces the rule that makes findings usable: a
// high-confidence diagnosis with nothing to check is worse than no diagnosis,
// because it costs an SRE the time to disprove it.
func TestEveryFindingCitesEvidence(t *testing.T) {
	for name := range fixtures {
		snap := load(t, name)
		for _, f := range All(snap) {
			if len(f.Evidence) == 0 {
				t.Errorf("%s: finding %q has no evidence", name, f.ID)
			}
			for i, e := range f.Evidence {
				if e.Source == "" || e.Ref == "" || e.Excerpt == "" {
					t.Errorf("%s: finding %q evidence[%d] is incomplete: %+v", name, f.ID, i, e)
				}
			}
			if f.Confidence <= 0 || f.Confidence > 1 {
				t.Errorf("%s: finding %q has confidence %v, want (0,1]", name, f.ID, f.Confidence)
			}
			if f.Title == "" || f.Detail == "" {
				t.Errorf("%s: finding %q is missing a title or detail", name, f.ID)
			}
		}
	}
}

// TestRankingIsWorstFirst pins the output order an SRE reads under pressure:
// severity dominates, and confidence only breaks ties within a severity. A
// 0.99-confidence warning must never outrank a 0.5-confidence critical.
func TestRankingIsWorstFirst(t *testing.T) {
	findings := []model.Finding{
		{ID: "c", Severity: model.Info, Confidence: 0.9},
		{ID: "a", Severity: model.Critical, Confidence: 0.5},
		{ID: "b", Severity: model.Critical, Confidence: 0.9},
		{ID: "d", Severity: model.Warning, Confidence: 0.99},
	}
	slices.SortStableFunc(findings, func(x, y model.Finding) int { return x.Less(y) })

	var got []string
	for _, f := range findings {
		got = append(got, f.ID)
	}
	if strings.Join(got, " ") != "b a d c" {
		t.Errorf("order = %v, want [b a d c] (severity first, then confidence)", got)
	}
}

// TestDeterministic guards the purity rule: detectors must be pure functions, or
// committed fixtures flap and the suite stops meaning anything.
func TestDeterministic(t *testing.T) {
	for name := range fixtures {
		snap := load(t, name)
		first := All(snap)
		for i := 0; i < 10; i++ {
			again := All(snap)
			if len(again) != len(first) {
				t.Fatalf("%s: run %d produced %d findings, first run produced %d",
					name, i, len(again), len(first))
			}
			for j := range again {
				if again[j].ID != first[j].ID {
					t.Fatalf("%s: run %d differs at %d: %s vs %s",
						name, i, j, again[j].ID, first[j].ID)
				}
			}
		}
	}
}

// TestRegistryIDsAreUnique catches a copy-paste that would make two detectors
// indistinguishable in the output.
func TestRegistryIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range IDs() {
		if seen[id] {
			t.Errorf("duplicate detector ID %q", id)
		}
		seen[id] = true
	}
}
