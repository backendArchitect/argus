package project

import (
	"strings"
	"testing"
)

// planted holds fake credentials shaped like the real thing. Every one must be gone from the output.
//
// They are deliberately NOT byte-accurate imitations of any vendor's live format: GitHub's push
// protection rejected an earlier version of this file for exactly that reason, which is the system
// working as intended. What each case has to exercise is one of OUR patterns, not a scanner's.
// These are the shapes real applications actually print: a startup banner echoing a DSN, an HTTP
// client logging its Authorization header, a config dump.
var planted = []struct {
	name, line, mustNotContain string
}{
	{
		"jwt in an auth header",
		`level=debug msg="calling upstream" authorization="Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"`,
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
	},
	{
		"postgres dsn with password",
		`starting up: DATABASE_URL=postgres://orders:hunter2CorrectHorse@db.internal:5432/orders?sslmode=require`,
		"hunter2CorrectHorse",
	},
	{
		"aws access key id",
		`aws sdk: using static credentials AKIAIOSFODNN7EXAMPLE for region us-east-1`,
		"AKIAIOSFODNN7EXAMPLE",
	},
	{
		"gcp api key",
		`maps client configured key=AIzaSyD-ExampleFakeKey1234567890abcdefgh`,
		"AIzaSyD-ExampleFakeKey1234567890abcdefgh",
	},
	{
		"github token",
		`git: remote auth ghp_1234567890abcdefghijklmnopqrstuvwxyz`,
		"ghp_1234567890abcdefghijklmnopqrstuvwxyz",
	},
	{
		"slack webhook token",
		`notifier: token xoxb-EXAMPLE-NOT-A-REAL-TOKEN-AAAA`,
		"xoxb-EXAMPLE-NOT-A-REAL-TOKEN-AAAA",
	},
	{
		"password in a config dump",
		`config: {"host":"db","password":"s3cr3t-p@ssw0rd","port":5432}`,
		"s3cr3t-p@ssw0rd",
	},
	{
		"api_key kv pair",
		`env: STRIPE_API_KEY=EXAMPLE-NOT-A-REAL-KEY-AAAAAAAA`,
		"EXAMPLE-NOT-A-REAL-KEY-AAAAAAAA",
	},
	{
		"private key header",
		`-----BEGIN RSA PRIVATE KEY-----`,
		"BEGIN RSA PRIVATE KEY",
	},
	{
		"bespoke high-entropy token",
		`internal auth: session=9xK2pQvR7mZ4wL8nT3bY6cF1dH5jS0aE2gU7iO9kP4rX`,
		"9xK2pQvR7mZ4wL8nT3bY6cF1dH5jS0aE2gU7iO9kP4rX",
	},
}

// TestRedactionRemovesPlantedCredentials is the gate SECURITY.md commits to. Logs are the single
// most likely place for a credential to reach model context, because an application can print
// anything and frequently does.
func TestRedactionRemovesPlantedCredentials(t *testing.T) {
	for _, tc := range planted {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactLine(tc.line)
			if strings.Contains(got, tc.mustNotContain) {
				t.Errorf("credential survived redaction\n  in:  %s\n  out: %s\n  leaked: %s",
					tc.line, got, tc.mustNotContain)
			}
			if !strings.Contains(got, redacted) {
				t.Errorf("nothing was redacted at all\n  in:  %s\n  out: %s", tc.line, got)
			}
		})
	}
}

// TestRedactionKeepsDiagnosticContext is the other half. Redaction that eats the whole line
// destroys the reason the logs were fetched — the reader must still be able to see WHAT was
// removed and where.
func TestRedactionKeepsDiagnosticContext(t *testing.T) {
	got := RedactLine(`starting up: DATABASE_URL=postgres://orders:hunter2CorrectHorse@db.internal:5432/orders`)
	for _, keep := range []string{"starting up", "postgres://", "db.internal", "5432"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction destroyed context %q: %s", keep, got)
		}
	}
}

// TestRedactionLeavesOrdinaryLogsAlone guards against the opposite failure. A redactor that fires
// on normal output blinds the diagnosis, and an over-eager one is discovered late because nobody
// notices absence.
func TestRedactionLeavesOrdinaryLogsAlone(t *testing.T) {
	clean := []string{
		`2026-08-17T09:14:22Z INFO  listening on :8080`,
		`panic: runtime error: invalid memory address or nil pointer dereference`,
		`goroutine 1 [running]:`,
		`github.com/backendArchitect/argus/internal/detect.detectOOMKill(0xc000123456)`,
		`	/src/internal/kube/gather.go:142 +0x1a5`,
		`GET /api/v1/orders/12345 200 14ms`,
		`level=warn msg="retrying" attempt=3 backoff=1.5s`,
		`image sha256:1f2e3d4c5b6a7988990a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60 pulled`,
		`connected to redis at redis-master.shop.svc.cluster.local:6379`,
	}
	for _, line := range clean {
		if got := RedactLine(line); got != line {
			t.Errorf("ordinary log line was altered\n  in:  %s\n  out: %s", line, got)
		}
	}
}

func TestShannonEntropy(t *testing.T) {
	// Random-looking base64 should be well above the threshold; English should be well below.
	if h := shannon("9xK2pQvR7mZ4wL8nT3bY6cF1dH5jS0aE2gU7iO9kP4rX"); h < entropyMin {
		t.Errorf("random token entropy %.2f, want >= %.2f", h, entropyMin)
	}
	if h := shannon("the quick brown fox jumps over the lazy dog"); h >= entropyMin+1 {
		t.Errorf("prose entropy %.2f is suspiciously high", h)
	}
}
