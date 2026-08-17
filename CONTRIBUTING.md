# Contributing to argus

Thanks for your interest in improving argus! It's a read-only Kubernetes
incident-diagnosis MCP server: one question in, one ranked diagnosis out. This
guide covers how to build it, test it, and get a change merged.

By participating you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Build from source

You need **Go 1.26+**. No C compiler, no cluster required to build or test.

```sh
git clone https://github.com/backendArchitect/argus
cd argus
go build ./...
go install .        # drops an `argus` binary in $GOBIN
```

## Run tests

```sh
go test ./...       # the whole suite — runs in well under a second
go test ./internal/project -run TestPodBudget -v
```

**The suite needs no cluster and must stay that way.** Detectors are pure
functions over a `model.Snapshot`, and snapshots are YAML fixtures in
`testdata/snapshots/`. If you find yourself needing a live cluster for a unit
test, the logic probably belongs on the pure side of the boundary.

Keep the suite green and fast. A change to non-trivial logic without a test
won't be merged.

## Lint

```sh
go vet ./...
gofmt -l .          # must print nothing; run `gofmt -w .` to fix
```

## Project structure

```
main.go                  CLI entry point: serve / capture / version
internal/
  model/                 Snapshot, Finding, Evidence — plain data, no clients
  kube/                  apiserver access: client, name resolution, concurrent gather
  project/               raw Kubernetes objects → allowlisted views; event dedup
  detect/                the detectors and the ranking
  tools/                 MCP tool definitions and server wiring
testdata/
  snapshots/             captured fixtures — the primary test gate
  broken/                deliberately-broken manifests that produce those fixtures
```

## Design principles

Four rules shape almost every decision here — please respect them in a PR.

### 1. The unit of work is a question, not a resource

One tool per SRE question. If you're adding `get_configmap`, stop: that shape
pushes correlation onto the model across many round-trips, which is the failure
mode argus exists to fix. Above roughly a dozen tools we've started mirroring
kubectl, and `TestToolSurface` will tell you so.

### 2. `model.Snapshot` is plain data

No client, no `context.Context`, no live API object may appear in `model`. The
Snapshot must round-trip through YAML — `TestSnapshotRoundTrip` enforces it.

This one constraint is load-bearing. It's what lets `argus capture` double as
the fixture generator, keeps the test suite clusterless and sub-second, and
guarantees production and tests run the same code path.

Store time as **seconds ago**, never as an absolute timestamp. A fixture with
absolute times rots silently: a detector asking "was this OOMKill recent?" stops
firing the day after capture, and the test still passes because nothing fired
and nothing was expected to.

### 3. Evidence is mandatory; confidence is honest

Every `Finding` must carry at least one `Evidence` entry. A high-confidence
diagnosis with nothing to check is worse than no diagnosis during an incident,
because it costs an SRE the time to disprove it.

Confidence is independent of severity, and a detector reading partial data
(check `Snapshot.Missing`) must lower its own. "We could not reach the metrics
API" must never render as "memory looks fine".

### 4. Context is a budget, not an afterthought

Projections are **allowlists, never blocklists** — a blocklist silently
regresses the moment Kubernetes adds a field. A projected pod stays under 400
tokens (`TestPodBudget`). Anything elided must be recorded in `Snapshot.Notes`;
silent truncation reads as "we looked at everything" when we didn't.

We also follow a "minimal, boring, deletion-over-addition" style: match the
naming and comment density of the surrounding code, and don't add abstractions
nobody asked for.

## Adding a detector

Detectors live in `internal/detect` and are pure functions:

```go
func(*model.Snapshot) []model.Finding
```

1. Write the detector. Read only from the Snapshot; no I/O, no clock, no
   randomness — it must be deterministic so fixtures don't flap.
2. Add a broken manifest to `testdata/broken/`, apply it to a `kind` cluster,
   and `argus capture` the result into `testdata/snapshots/`.
3. Add a test asserting your detector fires on that fixture **and that no other
   detector does**. False positives are the failure mode that destroys trust at
   3am, so the negative half of that assertion is the important half.
4. Give every emitted Finding real `Evidence` and an honest `Confidence`.

Consider whether your detector should *widen scope*. If three unrelated
Deployments are failing on one node, the finding is about the node — use
`Finding.Suppresses` to subsume the per-workload noise rather than reporting it
three times.

Open an issue first for anything that changes the tool surface, the Snapshot
format, or adds a heavy dependency.

## Commit format

We use [Conventional Commits](https://www.conventionalcommits.org):

```
feat(detect): add oomkill.limit-too-low
fix(project): key event dedup on owner, not pod name
docs(readme): document the call budget
perf(kube): drop template-level fields from per-pod projection
```

Type is one of `feat`, `fix`, `docs`, `perf`, `refactor`, `test`, `chore`.
Keep the subject imperative and under ~72 characters.

## Pull requests

- **Open an issue first** for features or anything changing the tool surface or
  Snapshot format. Bug fixes can go straight to a PR.
- **Keep it focused.** One logical change per PR.
- **Include a test** for non-trivial logic; `go test ./...`, `go vet ./...` and
  `gofmt -l .` must all be clean.
- **Explain the "why."** Describe the problem and how the change addresses it at
  the root cause, not the symptom.

## Security

argus reads production clusters and feeds the result to a language model. The
read-only guarantee, secret redaction, and the apiserver call budget are all
security-critical — please read [SECURITY.md](SECURITY.md) before touching
`internal/kube` or `internal/project`.

In particular: **do not widen the `allowed` list in `readonly_test.go` to make a
build pass.** That test is the only thing standing between a refactor and a
write against someone's production cluster. If you have a genuine read-only call
that shares a name with a mutating verb, add it with a written reason.

Please do not open public issues for security concerns — email
**chauhanvatsal55@gmail.com** instead.

## License

By contributing, you agree that your contributions will be licensed under the
project's [MIT License](LICENSE).
