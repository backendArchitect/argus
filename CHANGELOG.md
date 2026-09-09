# Changelog

All notable changes are noted here. The format loosely follows
[Keep a Changelog](https://keepachangelog.com), and the project uses
[Conventional Commits](https://www.conventionalcommits.org).

## Unreleased

argus is pre-release. Six tools work end to end, and v0.2 is complete apart from the
informer cache, which is deliberately unbuilt — see the note under Added.

### Added

- **Three more silent misses closed**, all of them cases where argus previously reported
  nothing about a workload that was plainly broken or plainly unmaintainable.

  **The missing ConfigMap or Secret consumed as a volume** rather than as environment.
  Reported under the same `config.missing-configmap` / `config.missing-secret` IDs as the
  env form, because the object to create and the way to create it are identical — only
  the line of the manifest to look at differs. It is found down a completely different
  path, though, and is the harder of the two to diagnose by hand: the env form states the
  problem on the container, while this one fails earlier in the kubelet's mount path and
  leaves the container on `ContainerCreating` with an **empty** status message. The only
  statement of the cause anywhere is a `FailedMount` event, which ages out after about an
  hour, after which the cluster retains no explanation of why the pod is stuck at all.
  The detector is therefore gated on the event rather than the state — `ContainerCreating`
  is the normal state for the first seconds of every pod's life — plus a requirement that
  the pod still be un-ready, since a pod that has since mounted and gone ready has had the
  problem fixed.

  **`image.invalid-reference`** — an image reference the kubelet cannot parse
  (`reason=InvalidImageName`), most often a second colon from a tag pasted onto an
  already-tagged image. Its own ID rather than a fifth pull cause, because it is not a
  pull failure: the kubelet rejects the string before contacting any registry, so there is
  no registry verdict to classify. Every other image finding sends you to the registry or
  its credentials; this one sends you to the manifest, and retrying is a wasted move.

  **`hpa.cannot-scale` and `pdb.blocks-disruption`** — the first detectors to read the HPA
  and PDB. Both objects were already being fetched and projected on **every** diagnosis and
  read by nothing at all, so the apiserver calls and the token budget were being spent to
  reach no conclusion. Neither had ever been exercised by a fixture either; the two new
  ones are the first to cover that gather path.

  `hpa.cannot-scale` is the autoscaler that is not autoscaling: `ScalingActive=False`, so
  no replica count is computed and the workload is pinned wherever it sits. Every object
  looks fine — the HPA exists, the Deployment is Ready, the replica count reads as
  deliberate — and the next traffic spike is an outage with no proximate cause in any log.

  `pdb.blocks-disruption` is the cause of the incident that does not look like one: a node
  drain, cluster upgrade or scale-down that hangs for hours while `kubectl drain` reports
  only that it cannot evict a pod. The health gate is the entire discriminator and skipping
  it would have made the detector actively harmful — **every** broken workload also has
  `disruptionsAllowed: 0`, which is the budget working exactly as intended, so firing on
  that would put a confident distraction on top of every real outage. It reports only when
  the workload has met its bar and the budget still leaves no headroom, which means the
  constraint is structural.

  Both are **warnings, not criticals**, deliberately: neither stops the workload serving,
  so neither may ever displace the critical that explains an actual outage.

  Six new fixtures, five of them captured from live clusters. One is a second silence
  control: `pdb-has-headroom` is the same shape as `pdb-blocks-drain` with a correctly
  sized budget, and must produce nothing. `healthy` cannot cover that — it has no PDB, so
  it proves only that the detector ignores absence, never that it does the arithmetic right
  when a budget is present and fine. Any detector whose subject is an optional object needs
  a control of that shape or its silence is untested.

- **A detector for the config reference that does not resolve** —
  `config.missing-configmap`, `config.missing-secret`, `config.missing-key`, and
  `config.missing-reference` as the honest fallback.

  This one closes a silent miss rather than adding a capability. A container whose
  `envFrom` or `valueFrom` names a ConfigMap or Secret that is not there produced
  **zero findings**: argus reported nothing wrong about a Deployment that could never
  start a single pod. It was invisible for a structural reason rather than a missing
  string — the crash-loop detector's start-failure reasons are *terminated*-state
  reasons, and this container never terminates. The kubelet retries forever, so it sits
  in `waiting` with `CreateContainerConfigError` and `restartCount` 0 for as long as the
  reference is broken, while `kubectl get pods` shows an unremarkable `0/1 Pending`.

  The four IDs exist because the fix lives in a different object in each case, which is
  the same reason `ImagePullBackOff` splits four ways. `config.missing-key` is the one
  worth having: the ConfigMap or Secret *exists*, so `kubectl get` and any reconciler
  watching it both look healthy, and it is one key inside it that the pod requires and
  the object does not define. A missing Secret gets its own caveat, because unlike a
  ConfigMap it is frequently created by a controller — sealed-secrets, external-secrets,
  a cloud CSI driver — and then the broken thing is the controller, not the reference.

  The finding carries **no log hint**, deliberately, and says so in words. Every other
  critical finding points at `get_workload_logs`; following that reflex here costs a
  round trip to discover an empty stream, because a container that was never created
  has never written anything. A test asserts the hint stays absent.

  Classification reads unstructured English out of the kubelet, so its wording is pinned
  by a table of messages copied verbatim from live clusters and checked on Kubernetes
  **1.25.3 and 1.35.5**, which produce byte-identical text. That table is the part a
  fixture cannot protect: a re-captured fixture would drift in the same direction as the
  change that broke the parsing.

- **`trace_service_path`** and **`argus trace`** — the Service is up, the pods are
  Ready, and traffic still does not arrive. Walks Ingress rule → selector → targetPort
  → endpoint addresses → readiness and reports the **first** hop that gives out.

  Only the first. Everything past a break is marked not-reachable rather than healthy,
  because a selector matching nothing guarantees no endpoints and nothing ready, and
  reporting those as three faults describes one fault three times.

  The case it exists for is the silent one: a `targetPort` naming a containerPort no
  container declares cannot be resolved by the endpoints controller, so the
  EndpointSlice is programmed with **no port**. The endpoint exists and reports Ready,
  the Deployment is 1/1, and `kubectl get svc,pods,ingress` shows nothing wrong — this
  is worse than a selector typo, where at least `kubectl get endpoints` shows `<none>`.
  Verified against live clusters on 1.25 and 1.35, both of which program `ports: null`.

  The named/numeric asymmetry is load-bearing: a **named** targetPort that matches
  nothing is a proven break, while a **numeric** one is only a warning, because
  `containerPort` is informational and a process may listen on a port it never
  declared. Reporting the numeric case as a fault would be a false positive on a
  legitimate manifest.

  A selector that matches nothing distinguishes a **label typo** from a workload that
  is not deployed here, by naming pods whose label value looks like a misspelling of
  what the selector wants. Four list calls, regardless of cluster size.

  Because the declared chain being intact is a common and useful result, the report
  names what it could not check — whether the process is really listening,
  NetworkPolicy, mesh sidecar mTLS, ingress controller health and class, DNS, the CNI
  dataplane — and gives `externalTrafficPolicy: Local` its own line when the Service
  sets it, since it drops traffic on nodes with no local ready pod while every hop
  above stays green.

- **The informer cache is argued against rather than deferred.** It was on the v0.2
  list to cut apiserver calls on repeat queries. It buys nothing for the CLI, which is
  short-lived by construction, and for `serve` it trades the call budget for a
  staleness window — a cache returning a 30-second-old pod state mid-rollout is worse
  than 13 live calls, because the answer is confidently wrong rather than slow. The
  measured cost does not justify it: 13 calls for a diagnosis, 10 for a 130-workload
  triage, against a budget of 60. Recorded here rather than dropped silently.

- **An end-to-end gate against live clusters** (`hack/e2e.sh`, plus a nightly CI
  matrix on Kubernetes v1.31 and v1.35). The unit suite replays snapshots captured
  on one cluster at one moment; it cannot notice upstream changing what it reports,
  which is the thing most likely to break detectors that key on reason strings. The
  gate applies `testdata/broken/` to a real cluster and asserts each detector still
  fires on the workload it is meant to, and stays silent on the healthy control.
  Behaviour, not bytes — a byte-diff would be red the moment CI ran a different
  version from the one the fixtures came from.
- **`TestPickContainer`** — unit coverage for the log-selection judgement (failing
  container over sidecar, previous instance on a crashloop, explicit overrides),
  which the live gate cannot assert without racing the pod.

- **`explain_pending`** and **`argus pending`** — why a workload's pods will not
  schedule. The scheduler reports a count ("0/1 nodes are available: 1 Insufficient
  memory"); this reports the arithmetic, per node: what the pod asked for, what the
  node has allocatable, how much is already reserved by other pods, and the shortfall.
  Requests rather than usage, because that is what the scheduler reserves against and
  reaching for usage is how the sum goes wrong by hand. Also distinguishes cordoned
  and not-Ready nodes, untolerated taints with full toleration matching semantics, and
  nodeSelector mismatches that name the label the node actually carries.

  The fit calculation is a pure function over a capacity table, so it is tested without
  a cluster — including the taint rule that PreferNoSchedule is a preference and never
  blocks, which if wrong would report a node as unusable that the scheduler would take.

  Every report names what argus did **not** evaluate: topology spread, inter-pod
  affinity, PV zone constraints, extended resources, pods-per-node. A pod declaring
  nodeAffinity gets a dedicated warning, because argus does not read affinity
  expressions and a node shown as fitting may still be excluded by one.

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

- **The service-path near-miss list over-matched on a shared label key.** Every pod in
  a namespace carries `app`, so "carries one of those label keys" named six unrelated
  workloads and pointed the reader at `bad-rollout` when the answer was `gapped`. Now
  restricted to label values that are plausibly a misspelling of what the selector
  wants. Same trap `relatedSelector` already documents, reached from the other
  direction — and caught the same way, by running against a live cluster rather than
  reasoning about it.
- **That near-miss count reported the length of its own truncated list**, so it said
  "6 pod(s)" while listing 14. A bounded list is fine; a bounded list whose count is
  the bound is a wrong number presented as a measurement.
- **The rollout detector missed stalled rollouts**, which is the shape a bad deploy
  usually has. It required the WORKLOAD to be degraded, so a deploy wedged behind
  `maxUnavailable` — new ReplicaSet unable to start, previous revision still serving
  every request, 2/2 ready at the Deployment — was reported as nothing wrong. A safe
  rollout strategy is *designed* to keep the workload up while the deploy fails, so
  gating on workload health suppressed the normal case. The condition is now that the
  current ReplicaSet has replicas it should be running and fewer are ready; requiring
  `Desired > 0` is what keeps the old rollback false positive dead. Severity now
  distinguishes the two: a wedged deploy that still serves everything grades warning,
  because calling it the same as an outage is how a severity scale stops meaning
  anything. Found by the new e2e gate on k8s 1.35 — every fixture, captured on 1.25,
  had agreed with the bug.

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
