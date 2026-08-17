# argus

![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)
[![License](https://img.shields.io/badge/License-MIT-green)](LICENSE)
![MCP](https://img.shields.io/badge/MCP-server-8A2BE2)
![Status](https://img.shields.io/badge/status-pre--release-orange)
[![CI](https://github.com/backendArchitect/argus/actions/workflows/ci.yml/badge.svg)](https://github.com/backendArchitect/argus/actions/workflows/ci.yml)
[![Release](https://github.com/backendArchitect/argus/actions/workflows/release.yml/badge.svg)](https://github.com/backendArchitect/argus/actions/workflows/release.yml)
![Read-only](https://img.shields.io/badge/cluster%20access-read--only-2ea44f)

### One question in, one ranked diagnosis out.

**argus** is a Kubernetes incident-diagnosis MCP server. You ask *"why is checkout-api broken?"* and
it answers with a ranked list of causes, each one citing the evidence it used.

Named for Argus Panoptes, the hundred-eyed watchman who never slept because only some of his eyes
closed at a time — it watches twelve resource kinds at once so you don't have to correlate them
by hand.

🌐 **[See it in action → backendarchitect.github.io/argus](https://backendarchitect.github.io/argus/)**

> **Status: pre-release.** `diagnose_workload` works end to end — six detectors, ranked findings,
> mandatory evidence — and is verified against fixtures captured from real clusters.
> `cluster_triage` is not built yet. See [Roadmap](#roadmap).

---

## Why this exists

Every Kubernetes MCP server available today is `kubectl` with a JSON schema stapled on:
`get_pods`, `describe_pod`, `get_logs`. That is worse than nothing, because it pushes the
correlation work onto the model across ten round-trips, each one dumping 8KB of `managedFields`
into the context window. By pod four the context is full of YAML noise and the model starts
guessing.

argus takes the opposite position: **the unit of work is a question, not a resource.**

| | Resource-shaped servers | argus |
|---|---|---|
| Tool surface | 40–100+ tools mirroring kubectl | 3 tools, one per SRE question |
| Correlation | the model does it, over many calls | done server-side, in one call |
| Output | raw objects | ranked findings with mandatory evidence |
| Context per pod | 2,000+ tokens | **under 400**, enforced by test |
| Repeated events | one line each | deduplicated, with the blast radius kept |
| Writes | usually available, sometimes gated | **none — no mutating call site exists** |

---

## What it does today

```sh
# Diagnose a workload from the terminal — the same pipeline the MCP tool uses.
argus diagnose checkout-api -n prod

# Run as an MCP server over stdio.
argus serve

# Logs for the failing container — previous instance if it is crashlooping.
argus logs checkout-api -n prod

# Collect a raw diagnosis snapshot as YAML (this is also the fixture generator).
argus capture deploy/checkout-api -n prod
```

A real diagnosis, captured verbatim:

```
DIAGNOSIS  Deployment argus-broken/oom-victim
replicas   0/1 ready, 1 updated, 0 available
findings   1 (1 critical)

1. [critical · confidence 71%] oomkill.limit-too-low
   Container "app" in oom-victim-7cb68bdb99 is being OOM-killed by its own memory limit
   The kernel killed container "app" for exceeding its memory limit of 32Mi. It is OOMKilled
   and not yet ready, so it has not recovered from the kill. Current usage is unavailable (the
   metrics API did not answer), so there is no measured basis for a new limit; size it from the
   workload's known working set.
   evidence:
     · pod/oom-victim-7cb68bdb99-4pmxn (pod.lastState):
         container "app" last terminated with reason=OOMKilled, exitCode=137, 76s ago
     · pod/oom-victim-7cb68bdb99-4pmxn (pod.spec):
         container "app" memory limit is 32Mi
     · pod/oom-victim-7cb68bdb99-4pmxn (pod.status):
         container "app" has restarted 4 times; current state is OOMKilled
   next: get_workload_logs(previous=true workload=oom-victim-7cb68bdb99) — the current container
         restarted after the kill, so the output leading up to it is in the previous instance

incomplete — these lookups failed, and any finding relying on them has had its confidence reduced:
  · metrics: the server could not find the requested resource (get pods.metrics.k8s.io)

(13 apiserver calls against kind-argus-test)
```

Note the confidence drop and the stated gap. That cluster had no metrics API, and saying so beats
implying memory looks fine.

Under the hood, one pass resolves a fuzzy name, fans out concurrently across the workload, its ReplicaSets, pods,
events, metrics, Services, endpoints, nodes, HPA and PDB, then projects all of it down to an
allowlisted, deduplicated snapshot. On a real production Deployment that is **13 apiserver calls
and ~3,300 tokens** — the same data unprojected runs to roughly 10,400.

The snapshot is plain YAML by design. That single constraint is what makes the test suite
possible: `capture` writes fixtures, tests replay them with no cluster at all, and production runs
the identical code path.

```
argus capture    →  gather → Snapshot → yaml            (writes testdata/snapshots/*.yaml)
go test          →  yaml   → Snapshot → detect → assert (no cluster, sub-second)
argus serve      →  gather → Snapshot → detect → rank   (production)
```

---

## Read-only, and not on the honour system

argus never writes to your cluster. That is not a convention or a flag — there is no mutating call
site in the binary, and a test walks the entire source tree's AST and fails the build on any call
to `Create`, `Update`, `Patch`, `Delete`, `Apply`, `Evict` or friends.

This matters more than it sounds. The usual advice is "enforce read-only at the RBAC layer, not in
your code" — correct for an in-cluster deployment, but **false for how you will actually run this**,
which is from your own kubeconfig, where RBAC grants you everything. RBAC will not hold that line,
so the binary holds it instead.

Two more limits are built into the client rather than bolted on:

- **A call budget.** A hard cap on apiserver requests per invocation, enforced in an
  `http.RoundTripper` so it counts every request and every retry. A diagnostic tool that DoSes the
  control plane during an incident is a career-limiting artifact.
- **A deadline with graceful degradation.** A slow cluster produces a partial snapshot that records
  what was missed, not a hung MCP session — and detectors reading partial data must lower their own
  confidence rather than quietly reasoning from absence.

Full threat model, including prompt injection via log and event content: [SECURITY.md](SECURITY.md).

---

## Install

Requires **Go 1.26+** and a working kubeconfig.

| Method | Command |
|---|---|
| **Go** | `go install github.com/backendArchitect/argus@latest` |
| **Docker** | see below — the image runs as nonroot, so the kubeconfig needs mounting deliberately |
| **Source** | `git clone https://github.com/backendArchitect/argus && cd argus && go install .` |

Prebuilt binaries (Linux/macOS/Windows · amd64 & arm64) are on the
[Releases page](https://github.com/backendArchitect/argus/releases).

### Updating

```sh
argus update           # replace this binary with the latest release
argus update -check    # just report whether one is available
```

The download is verified against the SHA-256 published beside it and the swap is
atomic, so a failed update leaves the working binary untouched. It refuses to
overwrite a binary you built from a clone — use `go install .` for that, or
`argus update -force` if you really mean it.

The container image is distroless and runs as a nonroot uid, so mount the
kubeconfig **file** and name it explicitly — mounting `~/.kube` as a directory
lands on a path the container user cannot read:

```sh
docker run --rm -u "$(id -u):$(id -g)" \
  -v ~/.kube/config:/kube/config:ro \
  ghcr.io/backendarchitect/argus \
  diagnose checkout-api -n prod --kubeconfig /kube/config
```

Note the apiserver must be reachable from inside the container: this works for a
real cluster, but not for a local `kind` cluster listening on the host's
`127.0.0.1` (add `--network host` for that).

Connect it to an AI editor:

```sh
claude mcp add argus -- argus serve
```

---

## Roadmap

**Working now**

- **`diagnose_workload`** — the flagship: one call in, a ranked diagnosis out
- **`get_workload_logs`** — picks the failing pod and container over sidecars, reads the
  *previous* instance on a crashloop, collapses repeated lines, redacts credentials, and budgets
  by tokens. Took a real crashloop from 206 lines to 7
- **`argus update`** — verified, atomic self-update
- **Seven detectors, 19 finding IDs** — crash loop (which distinguishes a container the runtime *cannot start* from
  one that starts and exits, and reads the exit code: wrong entrypoint, segfault, abort, exits-zero,
  SIGTERM) · OOM limit too low · bad rollout · image pull (four distinct causes) ·
  readiness misconfigured · endpoint gap (selector typo vs readiness failure) · node-caused,
  which *widens scope* and suppresses the per-workload symptoms it explains
- **The broken-fixture suite** — each fixture asserts its detector fires **and no others do**,
  with a healthy control that must produce nothing
- MCP server over stdio, with schemas derived from Go types
- Fuzzy workload resolution (Deployment / StatefulSet / DaemonSet / Argo Rollout), returning
  candidates on ambiguity rather than guessing
- Concurrent gather with a call budget, deadline, and degradation tracking
- Projection layer: allowlisted fields, per-pod token budget, event deduplication
- `argus capture` — snapshot to YAML, doubling as the fixture generator
- Read-only enforcement, verified by an AST test

**v0.1 — next.** One tool left: `cluster_triage` — what is broken right now, grouped by owner
rather than per pod, so forty crashlooping pods of one Deployment is one finding with a count.

**v0.2** — `explain_pending` (per-node fit arithmetic for unschedulable pods),
`trace_service_path`, informer cache, kind-based CI.
**v0.3** — GKE integrations (Cloud Logging fallback for dead pods, Autopilot, Managed Prometheus),
`compare_environments`, `check_reachability`, in-cluster deployment with Workload Identity.

**Folded in rather than built.** `diff_rollout` was planned as its own tool; the semantic
template diff it would have provided lives inside the `rollout.bad-template` detector instead, so
you get it as part of a diagnosis rather than as a separate call. Noted here because it vanished
from the plan without explanation otherwise.

**Later, maybe never** — mutations against a cluster. That is where the liability is; read-only
diagnosis is where nearly all the value is. Note the one exception already shipped: `argus update`
replaces argus's *own binary*, verified against a published checksum. It gives argus no ability to
write to a cluster.

---

## Learn more

- **[USAGE.md](USAGE.md)** — every command and flag, the snapshot format, MCP setup, how detectors work.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — build, test, add a detector (please read the [Code of Conduct](CODE_OF_CONDUCT.md)).
- **[CHANGELOG.md](CHANGELOG.md)** — what's changed. **[SECURITY.md](SECURITY.md)** — reporting a vulnerability.

**In one line:** argus is a read-only Kubernetes diagnosis MCP server that answers SRE questions
with ranked, evidence-backed findings instead of handing a model a pile of YAML.

## License

[MIT](LICENSE)
