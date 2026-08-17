package project

import (
	"math"
	"regexp"
	"strings"
)

// Redaction of log content.
//
// Projection keeps credentials out of *object* fields by never reading Secrets and never emitting
// env values. Logs are the other half of the problem and the harder one: an application can print
// anything, and applications print credentials constantly — a startup banner echoing its DSN, an
// HTTP client logging an Authorization header, a stack trace carrying a token in a URL.
//
// The rule here is to redact on shape rather than on context, because there is no context to
// rely on: a log line is an opaque string. False positives are acceptable — a redacted hash is a
// mild annoyance, a leaked production token in a model's context is not.

type pattern struct {
	name string
	re   *regexp.Regexp
	// group is the submatch index to replace; 0 replaces the whole match. Using a group keeps the
	// identifying prefix visible ("password=<redacted>") so a reader can still see WHAT was
	// redacted, which is most of the diagnostic value.
	group int
}

var patterns = []pattern{
	// Structured, high-confidence tokens first: these are unambiguous.
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`), 0},
	{"aws-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA)[0-9A-Z]{16}\b`), 0},
	{"gcp-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), 0},
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), 0},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`), 0},
	{"private-key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----`), 0},

	// Credentials embedded in a URL: postgres://user:password@host/db
	{"url-credentials", regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:@/\s]+:)([^@/\s]+)(@)`), 2},

	// Authorization headers.
	{"bearer", regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(\S+)`), 2},
	{"bearer-scheme", regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=-]{16,})`), 2},
	{"basic-scheme", regexp.MustCompile(`(?i)\b(basic\s+)([A-Za-z0-9+/=]{16,})`), 2},

	// key=value / "key": "value" for names that mean a secret. Deliberately broad on the key and
	// permissive on the value: this is the shape most application logs actually leak in.
	//
	// The key is allowed a leading identifier prefix rather than anchored with \b, because real
	// names are almost always prefixed — STRIPE_API_KEY, DB_PASSWORD, gcp.service.token — and
	// underscore is a word character, so \b never matches inside them.
	{"sensitive-kv", regexp.MustCompile(
		`(?i)([A-Za-z0-9_.-]*(?:pass(?:word|wd)?|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|credentials?|auth|dsn|connection[_-]?string))(["']?\s*[:=]\s*["']?)([^\s"',;&)}\]]{4,})`), 3},
}

const redacted = "<redacted>"

// RedactLine removes credential-shaped substrings from one line of log output.
func RedactLine(s string) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllStringFunc(s, func(m string) string {
			if p.group == 0 {
				return redacted
			}
			sub := p.re.FindStringSubmatch(m)
			if len(sub) <= p.group {
				return redacted
			}
			// Rebuild the match with only the secret group replaced, so the surrounding context
			// ("password=", "postgres://user:") survives and the reader knows what went missing.
			out := m
			if idx := strings.LastIndex(out, sub[p.group]); idx >= 0 {
				out = out[:idx] + redacted + out[idx+len(sub[p.group]):]
			}
			return out
		})
	}
	return redactHighEntropy(s)
}

// entropyMin is the Shannon entropy (bits per character) above which a long opaque token is
// treated as a secret. Base64-encoded random data sits around 5.5-6.0; English prose and Go
// identifiers sit well below 4. 4.2 catches real keys while leaving words, paths, and hex hashes
// that are merely long — like git SHAs — mostly alone.
const entropyMin = 4.2

// minEntropyLen is the length below which we do not guess. Short high-entropy strings are far more
// often abbreviations, IDs, or hashes than credentials, and redacting those destroys the log's
// diagnostic value for no security gain.
const minEntropyLen = 32

var tokenish = regexp.MustCompile(`[A-Za-z0-9+/=_-]{` + itoa(minEntropyLen) + `,}`)

// redactHighEntropy is the backstop for credentials that match no known vendor format — internal
// tokens, rotated keys, anything bespoke. It is deliberately the last resort, and deliberately
// conservative, because over-redaction blinds the very diagnosis logs are being fetched for.
func redactHighEntropy(s string) string {
	return tokenish.ReplaceAllStringFunc(s, func(m string) string {
		if looksLikeIdentifier(m) || shannon(m) < entropyMin {
			return m
		}
		return redacted
	})
}

// looksLikeIdentifier spares strings that are long and mixed but plainly not secrets: Go package
// paths, container IDs, image digests, and snake/kebab-cased names.
func looksLikeIdentifier(s string) bool {
	if strings.Count(s, "_") >= 2 || strings.Count(s, "-") >= 3 {
		return true
	}
	// Pure lowercase hex of a familiar length is a digest or a git SHA, not a credential.
	if len(s) == 40 || len(s) == 64 {
		hex := true
		for _, r := range s {
			if !strings.ContainsRune("0123456789abcdef", r) {
				hex = false
				break
			}
		}
		if hex {
			return true
		}
	}
	return false
}

// shannon returns entropy in bits per character.
func shannon(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// itoa avoids importing strconv for one constant used at package init.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
