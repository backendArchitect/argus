# Deliberately broken workloads

Each manifest here breaks in exactly one way. Applied to a throwaway cluster and
captured with `argus capture`, they become the snapshot fixtures in
`../snapshots/` that the detector tests replay with no cluster at all.

The suite's real assertion is the negative one: each fixture's detector must
fire **and no others may**. False positives are what destroy trust in a
diagnostic tool at 3am, so a detector that fires on five unrelated fixtures is
worse than one that fires on none.

## Regenerating fixtures

```sh
kind create cluster --name argus-test
kubectl apply -f testdata/broken/
# give the failures time to actually happen — backoff, probes, OOM kills
sleep 120
./hack/capture-fixtures.sh
```

## Why these are namespaced together

Everything lands in namespace `argus-broken`. A single flat namespace is
deliberate: it means the endpoint-gap and node-scope detectors see the same
neighbour workloads they would in a real namespace, rather than an artificially
clean one where a selector can only match its own pods.

## The manifests

| File | Breaks | Detector it should trigger |
|---|---|---|
| `oom-limit-too-low.yaml` | 32Mi limit, allocates 250Mi | `oomkill.limit-too-low` |
| `image-pull-typo.yaml` | image tag that does not exist | `image.pull-failed` |
| `readiness-too-fast.yaml` | probe deadline 2s, startup takes 30s | `probe.readiness-misconfigured` |
| `endpoint-gap.yaml` | Service selector has a typo | `endpoints.no-ready-backends` |
| `bad-rollout-v1.yaml` → `-v2.yaml` | healthy revision, then a broken one | `rollout.bad-template` |
| `healthy.yaml` | nothing — the control | *(none — this is the false-positive check)* |

`healthy.yaml` is the most important file here. Every detector runs against it,
and any detector that fires has a false positive. Without a healthy control the
suite only ever proves detectors are eager, never that they are discriminating.

## Not represented here

`node.unhealthy-host` needs a node reporting `MemoryPressure` or `DiskPressure`,
which cannot be induced reliably or safely in a local cluster. Its fixture is
hand-authored in `../snapshots/node-pressure.yaml` — legitimate precisely
because `model.Snapshot` is plain data with no client in it, so a
hand-written snapshot is indistinguishable from a captured one.

Note also that in a single-node kind cluster **every** pod shares a node. A node
detector keying on "several failing workloads co-located" would fire on this
whole directory. It must require actual node-level evidence — an abnormal node
condition — and the healthy control plus the other fixtures are what prove it
does.
