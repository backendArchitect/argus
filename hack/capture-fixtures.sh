#!/usr/bin/env bash
# Capture the deliberately-broken workloads in testdata/broken/ into snapshot
# fixtures that the detector tests replay with no cluster.
#
# Usage:  ./hack/capture-fixtures.sh [kube-context]
#
# Apply testdata/broken/ first and give the failures time to develop — backoff,
# probe failures and OOM kills are not instant, and a snapshot taken too early
# captures a workload that is merely starting rather than one that is broken.
set -euo pipefail

CONTEXT="${1:-kind-argus-test}"
NS=argus-broken
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/testdata/snapshots"

go build -C "$ROOT" -o "$ROOT/argus" .
mkdir -p "$OUT"

# workload:fixture-name
targets=(
  "oom-victim:oom-limit-too-low"
  "pull-typo:image-pull-typo"
  "slow-starter:readiness-too-fast"
  "gapped:endpoint-gap"
  "bad-rollout:bad-rollout"
  "noisy-crashloop:crashloop-nonzero"
  "bad-entrypoint:crashloop-wont-start"
  "healthy:healthy"
)

for t in "${targets[@]}"; do
  workload="${t%%:*}"
  fixture="${t##*:}"
  printf '%-18s -> %s.yaml\n' "$workload" "$fixture"
  "$ROOT/argus" capture "deploy/$workload" -n "$NS" --context "$CONTEXT" \
    > "$OUT/$fixture.yaml"
done

echo
echo "captured $(ls -1 "$OUT" | wc -l) fixtures into testdata/snapshots/"
echo "review the diff before committing — a fixture that changed unexpectedly is"
echo "either a real projection regression or genuine Kubernetes behaviour drift."
