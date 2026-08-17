#!/usr/bin/env bash
# Rebuild every snapshot fixture from scratch against a throwaway cluster.
#
# Usage:  ./hack/rebuild-fixtures.sh [kube-context]
#
# Deletes and recreates the namespace first. That matters more than it looks: a
# namespace reused across iterations accumulates ReplicaSet revisions and pull
# events from earlier attempts, and a fixture carrying that history can trip a
# detector for reasons that have nothing to do with what the fixture is testing.
# A fixture must isolate exactly one failure.
set -euo pipefail

CONTEXT="${1:-kind-argus-test}"
NS=argus-broken
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
K="kubectl --context $CONTEXT"

echo "==> resetting namespace $NS"
$K delete namespace "$NS" --ignore-not-found --wait=true
$K apply -f "$ROOT/testdata/broken/00-namespace.yaml"

echo "==> applying the single-failure workloads"
for f in oom-limit-too-low image-pull-typo readiness-too-fast endpoint-gap healthy; do
  $K apply -f "$ROOT/testdata/broken/$f.yaml"
done

# The rollout fixture needs a healthy revision in history before the broken one,
# because the detector's claim is "the new ReplicaSet is unhealthy and the
# previous one was healthy". Applying v2 alone yields a broken workload with no
# baseline, which is a different and much weaker diagnosis.
echo "==> rollout: applying healthy revision 1 and waiting for it"
$K apply -f "$ROOT/testdata/broken/bad-rollout-v1.yaml"
$K -n "$NS" rollout status deploy/bad-rollout --timeout=120s

echo "==> rollout: rolling to the broken revision 2"
$K apply -f "$ROOT/testdata/broken/bad-rollout-v2.yaml"

# Backoff, probe failures and OOM kills are not instant. A snapshot taken too
# early captures a workload that is merely starting rather than one that is
# broken, and the resulting fixture quietly tests nothing.
echo "==> waiting for failures to develop (this is not padding — backoff is real time)"
sleep 180

echo "==> current state"
$K -n "$NS" get pods

"$ROOT/hack/capture-fixtures.sh" "$CONTEXT"
