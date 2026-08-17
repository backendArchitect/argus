# argus — usage

The full reference. For the short version, see [README.md](README.md).

> **Pre-release.** This document describes what argus does **today**. Commands
> and tools that are designed but not yet built are marked *(planned)* and are
> listed in one place at the end — nothing above that section is aspirational.

---

## Contents

- [Commands](#commands)
- [Cluster flags](#cluster-flags)
- [Naming a workload](#naming-a-workload)
- [The snapshot](#the-snapshot)
- [Context budget](#context-budget)
- [Connect an AI editor (MCP)](#connect-an-ai-editor-mcp)
- [Logs](#logs)
- [Detectors](#detectors)
- [Safety limits](#safety-limits)
- [Fixtures and testing](#fixtures-and-testing)
- [Planned](#planned)

---

## Commands

```
argus serve                          MCP server over stdio (the default command)
argus diagnose <workload> -n <ns>    diagnose from the terminal, no MCP involved
argus logs     <workload> -n <ns>    logs for the failing container, with judgment
argus capture  <workload> -n <ns>    collect a raw snapshot, print YAML to stdout
argus version                        print the version
```

Running `argus` with no arguments is the same as `argus serve`.

### `argus serve`

Starts the MCP server, speaking JSON-RPC over stdin/stdout. Diagnostics go to
**stderr** — stdout carries the protocol framing and must not be written to by
anything else.

Accepts every [cluster flag](#cluster-flags). The cluster client is built per
tool call rather than at startup, so the server boots and answers `server_info`
even with no kubeconfig — a broken cluster config surfaces as one tool erroring,
not as a server that will not start.

### `argus diagnose`

Runs the full pipeline and prints the ranked diagnosis. Identical code path to
the `diagnose_workload` MCP tool — not a second implementation, so the two cannot
drift apart.

```sh
argus diagnose checkout-api -n prod
```

Useful for dogfooding, for CI, and for the case where you want the answer without
a model in the loop at all.

### `argus capture`

Resolves a workload, gathers everything relevant, and writes the projected
snapshot to stdout as YAML. A one-line summary goes to stderr, so redirecting
stdout gives you a clean file:

```sh
argus capture deploy/checkout-api -n prod > snapshot.yaml
# stderr: captured deployment/prod/checkout-api from gke_prod in 13 apiserver calls
```

This is also the fixture generator — see [Fixtures and testing](#fixtures-and-testing).

## Cluster flags

Accepted by every cluster-touching command:

| Flag | Default | Meaning |
|---|---|---|
| `-n`, `-namespace` | *(required)* | Namespace to resolve the workload in |
| `-kubeconfig` | standard rules, then in-cluster | Path to a kubeconfig file |
| `-context` | current-context | Kubeconfig context to use |
| `-timeout` | `10s` | Deadline for the whole gather |
| `-max-calls` | `60` | Hard cap on apiserver requests for this invocation |

Flags may appear before or after the positional argument. `argus capture foo -n prod`
and `argus capture -n prod foo` both work — Go's `flag` package stops at the
first positional by default, which would have silently ignored `-n` and read the
wrong namespace.

## Naming a workload

The workload argument is fuzzy. All of these can resolve to the same Deployment:

```sh
argus capture checkout            -n prod    # substring
argus capture checkout-api        -n prod    # exact name
argus capture deploy/checkout-api -n prod    # kind-qualified
```

Matching is **tiered** — exact name first, then prefix, then substring — and
stops at the first tier that produces a hit. An exact name is therefore never
made ambiguous by some unrelated workload that happens to contain it as a
substring.

If a query genuinely matches more than one workload, argus lists the candidates
rather than picking one:

```
argus: "orders" matches 2 workloads:
  deployment/shop/orders-api,
  statefulset/shop/orders-redis-master
```

Guessing wrong during an incident costs far more than one extra round-trip.

Kind prefixes: `deploy`/`deployment`, `sts`/`statefulset`, `ds`/`daemonset`,
`ro`/`rollout` (Argo). Rollouts are read through the dynamic client; if the CRD
isn't installed, they're skipped silently rather than reported as an error.

## The snapshot

One `capture` gathers all of the following, concurrently:

| Section | Contents |
|---|---|
| `workload` | spec, status, selector, conditions, generation vs observed generation |
| `replicasets` | the 3 newest, each with its projected pod template (for rollout diffs) |
| `pods` | per-container spec + status + **current metrics**, merged into one view |
| `events` | deduplicated and bucketed |
| `services` | selector, ports, and endpoint readiness |
| `nodes` | conditions and taints, for the nodes actually hosting these pods |
| `hpa`, `pdb` | scaling and disruption state |

Two fields describe the snapshot's own completeness, and the distinction between
them matters:

- **`degraded`** — steps that *failed or timed out*. A detector reading partial
  data must lower its confidence. "The metrics API was unreachable" must never
  render as "memory looks fine".
- **`notes`** — data deliberately *elided*, such as `replicasets: kept the 3
  newest of 11`. Kept separate so that trimming rollout history never makes a
  detector think the apiserver was unreachable.

A gather that partially fails still returns a snapshot. During an incident a
partial diagnosis beats no diagnosis — as long as the gaps are stated.

### Time is relative

Every timestamp is stored as **seconds ago**, never as an absolute time
(`created_seconds_ago`, `last_seen_seconds_ago`, `seconds_ago`).

This is what keeps committed fixtures meaningful. With absolute times, a fixture
rots silently: a detector asking "was this OOMKill recent?" stops firing the day
after capture, and the test still passes, because nothing fired and nothing was
expected to.

## Context budget

Raw Kubernetes objects are grotesque. A single Pod carries `managedFields`, a
full duplicate of its own spec under `last-applied-configuration`,
`resourceVersion`, generated token mounts, and more. Emitting that fills the
model's context with noise, and a model reasoning from noise guesses.

argus projects every object through an explicit **allowlist** — never a
blocklist, which would silently regress the moment Kubernetes adds a field.

- **A projected pod stays under 400 tokens**, enforced by `TestPodBudget`.
- Template-level fields (env keys, args, mounts) live on the ReplicaSet
  template, not repeated on every pod. Repeating them was a real 2.4× overrun:
  one production workload has 93 environment variables.
- Environment variable **values** are never emitted — only key names, which is
  all a detector needs, and the likeliest place for a credential to leak.

Measured on a real production Deployment:

| | Raw-ish | Projected |
|---|---|---|
| Snapshot | ~10,400 tokens | **~3,300** |
| Per pod | 946 tokens | **326** |
| Apiserver calls | — | 13 (budget 60) |

### Event deduplication

Events are normalized before grouping: UIDs, IP addresses, image digests,
timestamps, container IDs, durations and generated pod-name suffixes are all
replaced with placeholders. Groups are keyed on
`(type, reason, normalized message, kind)`.

The object name is deliberately **not** part of the key. Forty pods of one
Deployment reporting the same `BackOff` is one fact about the Deployment, not
forty facts. Each group reports:

```yaml
- type: Warning
  reason: BackOff
  message: Back-off restarting failed container app in pod checkout-api-<pod>
  count: 312          # total occurrences, honouring the apiserver's own series aggregation
  object_count: 40    # distinct pods affected — the blast radius
  object_name: checkout-api-7d9f-00001   # one example to drill into
  first_seen_seconds_ago: 900
  last_seen_seconds_ago: 12
```

Counts respect the event's own `count`/`series.count` rather than being
recounted locally — the apiserver already aggregates server-side, so recounting
would undercount by orders of magnitude on exactly the events that matter most.

## Connect an AI editor (MCP)

```sh
claude mcp add argus -- argus serve
```

For editors that take JSON config:

```json
{
  "mcpServers": {
    "argus": { "command": "argus", "args": ["serve"] }
  }
}
```

Verify the connection without touching a cluster. The `sleep` matters: `argus
serve` exits when stdin closes, so without it the pipe ends before the server
has written its replies and you get no output at all.

```sh
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"server_info","arguments":{}}}'
  sleep 0.5; } | argus serve 2>/dev/null
```

You should get two JSON-RPC responses, the second containing
`"read_only":true`.

### Tools

| Tool | Status | Purpose |
|---|---|---|
| `server_info` | **working** | Version, read-only status, available tools. Needs no cluster — a failure here is transport, not Kubernetes. |
| `diagnose_workload` | **working** | Ranked, evidence-backed diagnosis for one workload |
| `get_workload_logs` | **working** | Logs with judgment — see below |
| `cluster_triage` | *planned* | What is broken right now, grouped by owner |

Tool input and output schemas are generated from Go types, so they never drift
from the implementation.

## Logs

`get_workload_logs` and `argus logs` exist because fetching logs correctly during
an incident takes four decisions that are easy to get wrong under pressure.

**Which pod.** The least-ready one, then the most-restarted. A healthy replica's
logs say nothing about why its sibling is dying.

**Which container.** The one that is actually failing. Sidecars — `istio-proxy`,
`linkerd-proxy`, `cloudsql-proxy`, `otel-collector` and friends — are skipped
unless nothing else is present. Fetching the mesh proxy's access log while the
app container OOMKills is the most common way to waste a round-trip.

**Which instance.** On a crashlooping container argus reads the **previous**
one by default. The current instance is in backoff and has written nothing, so
its logs are empty and misleading; whatever explains the crash is in the instance
that died. Pass `previous` explicitly to override. If no previous instance exists
the call falls back to the current one and says so rather than failing.

**How much.** Output is capped by a **token** budget, not a line count — "the
last 100 lines" is meaningless when one line is a 4KB JSON blob and the next is
`ok`. When the budget binds, the **newest** lines are kept, because a failure is
at the end of a log and never the start, and the number elided is always
reported.

Lines identical after normalization collapse into one entry with a count. On a
real crashloop this took 206 lines to 7 — 200 "connection refused" lines that
each carried a *different* IP still collapsed, because normalization runs before
grouping:

```
LOGS  pod/noisy-crashloop-6bb4cd97fd-7b779  container=app  instance=PREVIOUS (the instance that died)
why   container "app" is the failing one (CrashLoopBackOff); reading the PREVIOUS
      instance because the current one has produced nothing (it last died with Error)

     starting up: DATABASE_URL=postgres://orders:<redacted>@db.internal:5432/orders
     config loaded: STRIPE_API_KEY=<redacted>
[x200] dial tcp 10.4.2.0:5432: connect: connection refused
     panic: runtime error: invalid memory address or nil pointer dereference
     goroutine 1 [running]:
     main.connect(0xc000123456)
     	/src/main.go:42 +0x1a5
```

### Credentials are redacted from log content

Projection keeps secrets out of object fields by never reading Secrets and never
emitting env values. Logs are the harder half: an application can print anything,
and applications print credentials constantly.

Redaction matches on **shape**, since a log line carries no context to reason
from — JWTs, AWS/GCP/GitHub/Slack key formats, private key headers, credentials
embedded in connection URLs, `Authorization` headers, and `key=value` pairs whose
key means a secret. A Shannon-entropy backstop catches bespoke tokens that match
no known vendor format.

Only the secret is replaced, not the line: `postgres://orders:<redacted>@db:5432`
tells you what went missing and where. Over-redaction blinds the diagnosis the
logs were fetched for, so the entropy backstop deliberately spares long hex
digests, git SHAs and snake-cased identifiers.

## Detectors

Each detector is a pure function over a `model.Snapshot`. Findings are ranked
severity-first, then by confidence — a 0.99-confidence warning never outranks a
0.5-confidence critical.

| ID | Fires when | What it adds beyond the symptom |
|---|---|---|
| `crashloop.container-wont-start` | Runtime reason `StartError` / `ContainerCannotRun` | Names the binary from the OCI error. The intuitive mapping is wrong: a typo'd entrypoint gives **`StartError` / exit 128**, not exit 127 — the container is never created, so the logs are empty and the emptiness is the confirmation. |
| `crashloop.command-not-found` / `-not-executable` | Exit 127 / 126 | Only when a shell actually ran. A manifest problem, not an application bug. |
| `crashloop.exits-successfully` | Exit 0 with restarts | Nothing is broken; the workload is the wrong shape. Work that finishes belongs in a Job. |
| `crashloop.segfault` / `.aborted` | Exit 139 / 134 | SIGSEGV / SIGABRT — a bug in the binary, so a rollback is the fastest mitigation. |
| `crashloop.terminated-on-signal` | Exit 143 | SIGTERM. Often a liveness probe killing a process that is alive but slow; cites the probe. |
| `crashloop.exiting-nonzero` | Any other non-zero exit, looping | Honest fallback at lower confidence: it is crashing, and only the logs say why. |
| `oomkill.limit-too-low` | `lastState.reason == OOMKilled` | The limit that killed it, and a sized suggestion when usage data exists. Keys on the reason, never on exit code 137 alone — 137 is just SIGKILL, which also covers liveness kills and evictions. |
| `oomkill.no-limit` | OOMKilled with no limit set | Different incident: the kill came from node pressure, not the container's own ceiling, so "raise the limit" would be wrong advice. |
| `rollout.bad-template` | New RS unhealthy, previous RS healthy | A semantic diff of the two pod templates. Requires a healthy predecessor — without one this is just "the workload is broken", which you knew. |
| `image.pull-not-found` | Registry reports no such tag | Distinguished from the three below, which look identical in the API and have completely different fixes. |
| `image.pull-unauthorized` | Credentials refused | Missing `imagePullSecret`, wrong registry, or expired creds — *not* a missing tag. |
| `image.pull-rate-limited` | Registry rate limit | Retrying will not help; authenticating or mirroring will. |
| `image.pull-failed` | Pull failed, cause unrecognised | Honest fallback at lower confidence rather than guessing "typo". |
| `endpoints.selector-matches-nothing` | Selector matches 0 pods | A label typo. The workload is healthy and `kubectl get` looks fine. |
| `endpoints.no-ready-backends` | Matches pods, none ready | A readiness problem, not a Service problem. |
| `probe.readiness-misconfigured` | Running, alive, never ready | Compares the probe's real deadline (`initialDelay + period × failureThreshold`) against observed uptime. Skips crashing containers — that is a different story with a different fix. |
| `node.unhealthy-host` | Node condition abnormal | **Widens scope** and suppresses the workload findings it explains. Requires an actual node condition: on a single-node cluster every pod shares a node, so co-location alone carries no information. |

### A crash loop must be looping now

Every detector that reads a *past* container death shares one gate: the failure has
to still be happening. Two real false positives came from getting this wrong —
an OOMKill from 18 days ago on a healthy pod reported as `critical`, and a node's
containerd churn restarting every pod with exit 255 which briefly made a healthy,
serving workload look like a crash loop.

So `crashloop.*` requires the container to be **in backoff or terminated right
now**. A container that is running has already come back, whatever it did earlier;
and one that is running but not ready is a readiness problem, which
`probe.readiness-misconfigured` owns. Historical restart counts deliberately gate
nothing — they say nothing about the current state, and using them made detectors
go silent during infrastructure churn.

### Confidence is honest

Confidence is independent of severity, and a detector reading partial data lowers
its own. If the metrics API is unreachable, the OOM finding drops from 0.95 to
0.71 and says so in prose. "We could not see it" must never render as "it looks
fine".

## Safety limits

### Read-only, enforced by the build

argus has no mutating call site. `TestNoMutatingVerbs` walks the entire
`internal/` AST and fails on any call to `Create`, `Update`, `UpdateStatus`,
`Patch`, `Apply`, `Delete`, `DeleteCollection` or `Evict`.

The usual advice — enforce read-only via RBAC, not in code — is right for an
in-cluster deployment and **wrong for how you will actually run this**. Run from
your own kubeconfig, RBAC grants you everything; the binary is the only thing
holding the line.

### Call budget

`-max-calls` (default 60) is enforced inside an `http.RoundTripper`, so it
counts every request from every client and every retry — something a counter
wrapped around call sites would miss. Past the cap, requests fail fast with a
message pointing at `-namespace` or a higher cap.

A diagnostic tool is run *during* an incident, when the control plane is already
under stress. Fan-out is bounded to 8 concurrent calls for the same reason.

### Untrusted content

Log lines and event messages contain user-controlled strings. Content fetched
from a cluster is framed as untrusted data in tool results, and because there is
no mutation path, injection can at worst mislead a diagnosis — never cause an
action. See [SECURITY.md](SECURITY.md).

## Fixtures and testing

The whole suite runs **without a cluster** in well under a second:

```sh
go test ./...
```

That's possible because `model.Snapshot` is plain data that round-trips through
YAML, which makes one code path serve three purposes:

```
argus capture    →  gather → Snapshot → yaml            (writes testdata/snapshots/*.yaml)
go test          →  yaml   → Snapshot → detect → assert (no cluster, sub-second)
argus serve      →  gather → Snapshot → detect → rank   (production)
```

To add a fixture:

```sh
kind create cluster --name argus-test
kubectl apply -f testdata/broken/oom-limit-too-low.yaml
argus capture deploy/oom-victim -n default > testdata/snapshots/oom-limit-too-low.yaml
```

Each fixture asserts that its detector fires **and that no others do**. The
negative half is the important half: false positives are what destroy trust in a
diagnostic tool at 3am.

Notable guard tests, if one fails and you're wondering why it exists:

| Test | Guards |
|---|---|
| `TestNoMutatingVerbs` | the read-only guarantee |
| `TestSnapshotRoundTrip` | fixtures can't silently lose a field |
| `TestPodBudget` | the 400-token-per-pod ceiling |
| `TestNoEnvValuesEverEmitted` | env values never reach model context |
| `TestToolSurface` | the tool count stays question-shaped, not kubectl-shaped |
| `TestEventsCollapse` | dedup actually collapses, and keeps the blast radius |

## Planned

Not built yet. Listed so the design is legible, not to imply availability.

**v0.1** — `cluster_triage`: "what is broken right now", grouped by owner rather
than per pod, so forty crashlooping pods of one Deployment is one finding with a
count instead of forty findings.

**v0.2** — `explain_pending` (per-node fit arithmetic: which nodes were excluded
by taints, by affinity, by insufficient CPU/memory with the actual numbers),
`trace_service_path`, an informer cache, and kind-based CI.

**v0.3** — GKE integrations (Cloud Logging fallback for evicted pods whose
kubelet logs are gone, Autopilot constraints, Managed Prometheus for historical
trends), `compare_environments`, `check_reachability`, and in-cluster deployment
with Workload Identity.

**Later, maybe never** — mutations. That is where the liability is, and
read-only diagnosis is where nearly all the value is.
