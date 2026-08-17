# Changelog

All notable changes are noted here. The format loosely follows
[Keep a Changelog](https://keepachangelog.com), and the project uses
[Conventional Commits](https://www.conventionalcommits.org).

## Unreleased

argus is pre-release, and the v0.1 tool surface is complete: `diagnose_workload`,
`get_workload_logs` and `cluster_triage` all work end to end.

### Added

- **`cluster_triage`** and **`argus triage`** — what is broken right now, across a
  namespace or the whole cluster. It deliberately does not loop `diagnose_workload`:
  that would have cost ~1,700 apiserver calls on a 165-workload cluster against a
  budget of 60, issued exactly when the control plane is already struggling. The data
  flow is inverted into a fixed handful of cluster-wide list calls, and the detectors
  are reused unchanged. Measured at 130 workloads in 10 calls, constant in cluster
  size. Grouped by controller, never by pod; infrastructure findings such as an
  unhealthy node are collapsed once with a count of affected workloads rather than
  repeated on every workload that node hosts.

- **`argus update`** — verified, atomic self-update, plus `-check` and `-force`.
  The published SHA-256 must be fetched and must match or the update is abandoned;
  HTTPS is required across redirects; only one file is taken from the archive by
  exact base name so a crafted tarball cannot place anything; the swap is a rename
  on the same filesystem so a failure leaves the working binary intact; and a
  clone-built binary is never overwritten without `-force`.
- **Real CLI help.** `argus --help` previously fell through to the serve flagset,
  printed `Usage of serve:` and exited 1 with `flag: help requested` leaking out —
  help read as a crash and the commands were undiscoverable. Now there is a
  top-level usage, per-command help with worked examples, and exit 0 throughout.
  The dispatch table and help text are one source so they cannot drift. A bare
  `argus` prints help rather than silently waiting on stdin.
- **Crash-loop detection**, the most common Kubernetes failure and previously a
  silent gap. Reads the exit code rather than restating CrashLoopBackOff: a broken
  entrypoint, a container that exits zero and should be a Job, SIGSEGV/SIGABRT, and
  SIGTERM from a liveness probe all get their own diagnosis and remedy.
- **`[minor]` / `[major]` in a commit message** now bump the release accordingly;
  the workflow could previously only patch-bump.

- **`get_workload_logs`** and **`argus logs`** — logs with judgment. Picks the
  least-ready pod, the failing container rather than a sidecar, and on a
  crashlooping container reads the *previous* instance by default, because the
  current one is in backoff and has written nothing. Falls back and says so when
  no previous instance exists.
- **Log redaction.** Credentials are matched on shape — JWTs, AWS/GCP/GitHub/Slack
  key formats, private key headers, credentials inside connection URLs,
  Authorization headers, and key=value pairs whose key means a secret — with a
  Shannon-entropy backstop for bespoke tokens. Only the secret is replaced, so
  `postgres://orders:<redacted>@db:5432` still says what went missing and where.
  Verified against ten planted credentials, and against ordinary log lines that
  must survive untouched.
- **Log grouping and a token budget.** Lines identical after normalization
  collapse to one entry with a count; output is capped by tokens rather than
  lines, and the newest lines are kept because a failure is at the end of a log.
  On a real crashloop: 206 lines to 7, including 200 "connection refused" lines
  that each carried a different IP.

- **`diagnose_workload`** and **`argus diagnose`** — the flagship tool and its CLI
  equivalent, sharing one code path so the two cannot drift. Returns both a
  rendered diagnosis and structured findings: models reason better from prose,
  and the struct is there for anything programmatic.
- **Six detectors**, each a pure function over a snapshot: `oomkill.limit-too-low`
  (plus `oomkill.no-limit`, a different incident with different advice),
  `rollout.bad-template` with a semantic template diff, four distinct
  `image.pull-*` causes that look identical in the API, `endpoints.*` separating a
  selector typo from a readiness failure, `probe.readiness-misconfigured`, and
  `node.unhealthy-host`, which **widens scope** and suppresses the workload-level
  symptoms it explains.
- **The broken-fixture suite.** Six workloads that each break in exactly one way,
  captured off a live cluster by `hack/rebuild-fixtures.sh`, plus a hand-authored
  node-pressure snapshot. Every fixture asserts its detector fires **and that no
  others do**, and a healthy control asserts nothing fires at all — the assertion
  that actually keeps the engine trustworthy.
- **Landing page** at `docs/`, published by GitHub Pages. Self-contained, motion
  respecting `prefers-reduced-motion`, and readable with JavaScript disabled.
- **Auto-release** on push to `main`, gated on the fixture suite, the read-only
  guarantee, and a smoke test that the built binary actually completes an MCP
  handshake. Cross-compiled binaries plus a distroless GHCR image; the version is
  stamped via `-ldflags` so a released binary no longer reports `0.1.0-dev`.
- **Dependabot** for the k8s and MCP dependency groups.
- **MCP server over stdio** on the official
  [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk)
  v1.7.0, with tool input/output schemas generated from Go types so they can't
  drift from the implementation. `server_info` is the handshake canary — it needs
  no cluster, so a failure there is transport rather than Kubernetes.
- **`argus capture`** — resolve a workload, gather concurrently, print a
  projected snapshot as YAML. Doubles as the fixture generator, which is what
  keeps the test suite clusterless.
- **Fuzzy workload resolution** across Deployment / StatefulSet / DaemonSet /
  Argo Rollout. Tiered matching (exact → prefix → substring) stops at the first
  tier that hits, so an exact name is never made ambiguous by an unrelated
  substring match. Genuine ambiguity returns the candidate list rather than
  guessing.
- **Concurrent gather** across workload, ReplicaSets, pods, events, metrics,
  Services, EndpointSlices, nodes, HPA and PDB. Per-container spec, live status
  and current metrics are merged into a single view — the three things every
  detector needs to correlate, which kubectl makes you fetch separately.
- **Projection layer** with explicit field allowlists, a 400-token-per-pod
  budget, and event deduplication.
- **Read-only enforcement as a build gate.** `TestNoMutatingVerbs` walks the
  whole `internal/` AST and fails on any mutating client verb, with a companion
  test proving the walk can actually fail. RBAC does not protect the primary
  deployment mode — argus runs from an operator's own kubeconfig, where RBAC
  grants full write access — so the binary holds the line instead.
- **Apiserver call budget** enforced in an `http.RoundTripper`, counting every
  request and retry, plus a per-invocation deadline and fan-out bounded to 8.
- **Graceful degradation**: a gather that partially fails still returns a
  snapshot. `degraded` records what failed (detectors must dock confidence);
  `notes` records what was deliberately elided. Kept separate so that trimming
  rollout history never reads as an unreachable apiserver.

### Fixed

Six defects caught by the test suite and the fixtures before any of this ran in anger:

- **`relatedSelector` excluded the very Services the endpoint detector exists to
  find.** It required a shared selector key *and value*, but a typo'd selector
  shares no pair with its workload — so a broken Service never reached the
  snapshot and the detector reported nothing. A shared key plus a matching name
  now also counts.
- **Stat counters on the landing page could freeze mid-animation**, displaying a
  partial number (47) where the real measurement is 326. A stalled animation is
  cosmetic; a wrong number presented as a measurement is not.
- **Event dedup keyed on the pod name**, so 40 crashlooping pods of one
  Deployment produced 40 near-identical groups — defeating the entire feature.
  Now keyed on `(type, reason, normalized message, kind)`, with `object_count`
  preserving the blast radius and a deterministic example pod to drill into.
- **Per-pod projection repeated template-level fields.** One real workload's 93
  environment variable keys were emitted on every pod, a 2.4× budget overrun on
  its own. Env keys, args and mounts now live only on the ReplicaSet template,
  where the rollout diff reads them. Real snapshot: 946 → 326 tokens per pod,
  ~10,400 → ~3,300 tokens overall.
- **Flags after a positional argument were silently ignored.** Go's `flag`
  package stops parsing at the first non-flag argument, so
  `argus capture foo -n prod` — the kubectl-style ordering everyone types —
  dropped `-n` and would have read the wrong namespace during an incident.
- **The image-digest normalizer never matched**, because a trailing `\b` cannot
  hold where a digest butts against a closing quote.

### Security

- Kubernetes `Secret` objects are never read.
- Environment variable *values* are never projected — only key names, verified
  by `TestNoEnvValuesEverEmitted`. The auto-injected service account token mount
  is elided.
- No mutation path exists, which is the load-bearing mitigation against prompt
  injection via log and event content: injection can mislead a diagnosis, but
  cannot cause an action.

See [SECURITY.md](SECURITY.md) for the full threat model.
