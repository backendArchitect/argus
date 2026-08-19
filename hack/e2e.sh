#!/usr/bin/env bash
# End-to-end gate: apply the deliberately broken workloads to a real cluster and assert
# every detector still fires on the one it is meant to, and stays silent on the healthy
# control.
#
# Usage:  ./hack/e2e.sh [kube-context]
#
# This asserts BEHAVIOUR, not captured bytes. A byte-diff against the committed
# snapshots would be red the moment CI runs a different Kubernetes version from the one
# the fixtures were captured on — and the thing worth protecting is not the YAML, it is
# that "reason=StartError means the container never started" is still true on the
# version people actually run.
set -uo pipefail

CONTEXT="${1:-kind-argus-test}"
NS=argus-broken
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
K="kubectl --context $CONTEXT"
ARGUS="$ROOT/argus"

fail=0
# flat collapses the renderer's line wrapping, so an assertion can match a phrase without
# depending on where the column limit happens to fall. Without this, "looks like a misspelling"
# failed to match output that plainly contained it, split across two lines.
flat() { tr '\n' ' ' | tr -s ' '; }
step() { printf '\n\033[1m── %s\033[0m\n' "$1"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; fail=1; }

step "building"
go build -C "$ROOT" -o "$ARGUS" . || exit 1
"$ARGUS" version

step "server version under test"
$K version -o json 2>/dev/null | grep -oE '"gitVersion": "v[^"]+"' | tail -1 || true

step "applying the broken workloads"
$K apply -f "$ROOT/testdata/broken/00-namespace.yaml" >/dev/null
for f in oom-limit-too-low image-pull-typo readiness-too-fast endpoint-gap healthy \
         noisy-crashloop bad-entrypoint unschedulable port-name-typo; do
  $K apply -f "$ROOT/testdata/broken/$f.yaml" >/dev/null
done
# The rollout fixture needs a healthy revision in history first: the detector's claim is
# "the new ReplicaSet is unhealthy and the previous one was healthy", which is only
# checkable if a healthy predecessor exists.
$K apply -f "$ROOT/testdata/broken/bad-rollout-v1.yaml" >/dev/null
$K -n $NS rollout status deploy/bad-rollout --timeout=180s >/dev/null || true
$K apply -f "$ROOT/testdata/broken/bad-rollout-v2.yaml" >/dev/null
rollout_applied=$SECONDS

# Backoff, probe failures and OOM kills take real elapsed time, so this polls rather
# than sleeping a guessed amount.
#
# Crucially it waits for the PRECONDITION EACH DETECTOR NEEDS, not for a status string.
# The first version grepped for 'CrashLoopBackOff|Error', which on k8s 1.35 matched
# 'RunContainerError' immediately — at restartCount 0 — while the crash-loop detector
# deliberately requires two restarts, because a single restart is not a loop. The gate
# asserted before its own precondition held and reported a passing detector as broken.
# A test that races the thing it measures is worse than no test: it teaches you to
# distrust real failures.
step "waiting for the failures to actually happen"
restarts() { # restarts <label> — total restarts across that workload's pods, 0 if none
  $K -n $NS get pods -l "app=$1" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[*].restartCount}{"\n"}{end}' 2>/dev/null \
    | awk '{s+=$1} END{print s+0}'
}
waiting_reason() { # waiting_reason <label>
  $K -n $NS get pods -l "app=$1" \
    -o jsonpath='{.items[*].status.containerStatuses[*].state.waiting.reason}' 2>/dev/null
}
phase() { $K -n $NS get pods -l "app=$1" -o jsonpath='{.items[*].status.phase}' 2>/dev/null; }

deadline=$(( SECONDS + 300 ))
while [ $SECONDS -lt $deadline ]; do
  ready=0
  # These three need >= 2 restarts before their detectors will speak.
  [ "$(restarts oom-victim)"      -ge 2 ] && ready=$((ready+1))
  [ "$(restarts noisy-crashloop)" -ge 2 ] && ready=$((ready+1))
  [ "$(restarts bad-entrypoint)"  -ge 2 ] && ready=$((ready+1))
  # An image that cannot be pulled never restarts; the waiting reason is the signal.
  case "$(waiting_reason pull-typo)" in *ImagePull*|*ErrImage*) ready=$((ready+1));; esac
  case "$(phase too-big)"           in *Pending*)               ready=$((ready+1));; esac
  # slow-starter must be up longer than its 2s probe deadline to be diagnosable.
  [ "$SECONDS" -ge 30 ] && ready=$((ready+1))
  # The rollout detector deliberately ignores a rollout younger than 60s, because every
  # normal deploy looks briefly like a failing one and a tool that cries wolf during
  # deploys gets muted. So the gate has to outwait that grace period rather than race it.
  [ $(( SECONDS - rollout_applied )) -ge 70 ] && ready=$((ready+1))
  [ "$ready" -ge 7 ] && break
  sleep 10
done
echo "  waited $((SECONDS))s; pod states:"
$K -n $NS get pods --no-headers | sed 's/^/    /'

step "each detector fires on the workload it is meant to"
# workload → the finding ID that must appear
assert_finding() {
  local workload="$1" want="$2"
  local out
  out="$("$ARGUS" diagnose "$workload" -n "$NS" --context "$CONTEXT" 2>&1)"
  if grep -q "$want" <<<"$out"; then
    ok "$workload → $want"
  else
    bad "$workload → expected $want, got:"
    sed 's/^/      /' <<<"$out" | head -8
  fi
}
assert_finding oom-victim      'oomkill.limit-too-low'
assert_finding bad-rollout     'rollout.bad-template'
assert_finding pull-typo       'image.pull-'
assert_finding gapped          'endpoints.selector-matches-nothing'
assert_finding slow-starter    'probe.readiness-misconfigured'
assert_finding noisy-crashloop 'crashloop.exiting-nonzero'
assert_finding bad-entrypoint  'crashloop.container-wont-start'

step "the healthy control stays silent"
out="$("$ARGUS" diagnose healthy -n "$NS" --context "$CONTEXT" 2>&1)"
if grep -q 'No findings' <<<"$out"; then
  ok "healthy → no findings"
else
  bad "healthy produced findings — a false positive:"; sed 's/^/      /' <<<"$out" | head -12
fi

step "explain_pending does the arithmetic"
out="$("$ARGUS" pending too-big -n "$NS" --context "$CONTEXT" 2>&1)"
if grep -q 'insufficient memory' <<<"$out" && grep -qE 'needs .*free of' <<<"$out"; then
  ok "too-big → insufficient memory, with the numbers"
else
  bad "too-big → expected per-node memory arithmetic:"; sed 's/^/      /' <<<"$out" | head -10
fi
out="$("$ARGUS" pending wrong-selector -n "$NS" --context "$CONTEXT" 2>&1)"
if grep -q 'nodeSelector wants' <<<"$out"; then
  ok "wrong-selector → nodeSelector mismatch, naming the label"
else
  bad "wrong-selector → expected a nodeSelector reason:"; sed 's/^/      /' <<<"$out" | head -10
fi

step "trace_service_path finds the hop that breaks"
# The headline case, and the one this tool exists for: every object reports healthy and the
# EndpointSlice is programmed with no port. Asserted here rather than only in a unit test
# because the claim is about what the ENDPOINTS CONTROLLER does with an unresolvable named
# targetPort — that is upstream behaviour, and a table test can only assert our reading of it.
out="$("$ARGUS" trace port-typo -n "$NS" --context "$CONTEXT" 2>&1)"
flatout="$(flat <<<"$out")"
if grep -q 'breaks at "target-port"' <<<"$flatout" && grep -q 'declared names: web' <<<"$flatout"; then
  ok "port-typo → target-port, naming the port the container actually declares"
else
  bad "port-typo → expected a target-port break:"; sed 's/^/      /' <<<"$out" | head -12
fi
# The cluster's own confirmation. If a future Kubernetes starts programming a port anyway, this
# is the line that notices, and the detector's reasoning would need revisiting.
if grep -q 'EndpointSlices carry no port at all' <<<"$flatout"; then
  ok "the EndpointSlice carries no port, which is upstream agreeing"
else
  bad "expected the dataplane confirmation; the endpoints controller may have changed behaviour"
fi

# A selector typo must be reported as a typo, naming only the mislabelled pod. An earlier version
# matched on a shared label KEY and named six unrelated workloads, because every pod carries `app`.
out="$("$ARGUS" trace gapped -n "$NS" --context "$CONTEXT" 2>&1)"
flatout="$(flat <<<"$out")"
if grep -q 'breaks at "selector"' <<<"$flatout" && grep -q 'looks like a misspelling' <<<"$flatout"; then
  ok "gapped → selector typo, distinguished from a missing workload"
else
  bad "gapped → expected a selector break naming the near miss:"; sed 's/^/      /' <<<"$out" | head -12
fi
if grep -qE 'misspelling of it: gapped-[a-z0-9]+' <<<"$flatout" \
   && ! grep -qE 'misspelling of it:[^.]*(bad-rollout|oom-victim|bad-entrypoint)' <<<"$flatout"; then
  ok "the near miss names the mislabelled pod and nothing else"
else
  bad "near-miss list over-matched — it should name only plausibly-mistyped values:"
  sed 's/^/      /' <<<"$out" | head -12
fi

# The healthy control must not produce a break. This is the assertion that keeps the tool
# trustworthy: a trace that finds a fault in a working Service is worse than no trace.
out="$("$ARGUS" trace healthy -n "$NS" --context "$CONTEXT" 2>&1)"
flatout="$(flat <<<"$out")"
if ! grep -q 'breaks at' <<<"$flatout"; then
  ok "healthy → no break found"
else
  bad "healthy Service reported a break:"; sed 's/^/      /' <<<"$out" | head -12
fi
# And an intact chain has to point somewhere, or it is a dead end.
if grep -q 'NetworkPolicy' <<<"$flatout"; then
  ok "an intact chain still names what it could not check"
else
  bad "intact chain named no gaps, which makes it a dead end"
fi

step "cluster_triage groups by controller and excludes the healthy one"
out="$("$ARGUS" triage -n "$NS" --context "$CONTEXT" 2>&1)"
if grep -q 'oom-victim' <<<"$out" && ! grep -qE '^Deployment .*/healthy ' <<<"$out"; then
  ok "triage lists broken workloads and omits healthy"
else
  bad "triage output unexpected:"; sed 's/^/      /' <<<"$out" | head -20
fi

step "get_workload_logs redacts, collapses, and explains its choice"
# Deliberately does NOT assert which instance was read. A crashlooping container flips
# between backoff and running, and when it is running its current output is genuinely
# the right thing to show — so asserting PREVIOUS here raced the pod and failed on a
# correct result. That decision is deterministic given a pod, so it belongs in a unit
# test (TestPickContainer); what is invariant on a live cluster is the rest.
out="$("$ARGUS" logs noisy-crashloop -n "$NS" --context "$CONTEXT" 2>&1)"
problems=""
grep -q '<redacted>'  <<<"$out" || problems="$problems no-redaction-marker"
grep -q 'hunter2'     <<<"$out" && problems="$problems CREDENTIAL-LEAKED"
grep -q 'STRIPE_API_KEY=EXAMPLE' <<<"$out" && problems="$problems api-key-not-redacted"
grep -qE '^\[x[0-9]+\]'  <<<"$out" || problems="$problems no-line-collapsing"
grep -q '^why '       <<<"$out" || problems="$problems no-explanation-of-choice"
if [ -z "$problems" ]; then
  ok "logs → credentials redacted, repeats collapsed, choice explained"
else
  bad "logs invariants violated:$problems"; sed 's/^/      /' <<<"$out" | head -14
fi

step "the MCP server answers over stdio"
out="$({ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"server_info","arguments":{}}}'
  sleep 2; } | "$ARGUS" serve --context "$CONTEXT" 2>/dev/null)"
if grep -q '"read_only":true' <<<"$out"; then ok "server_info reports read_only"; else bad "MCP handshake failed"; fi

printf '\n'
if [ "$fail" -eq 0 ]; then
  printf '\033[32mE2E PASSED\033[0m against %s\n' "$CONTEXT"
else
  printf '\033[31mE2E FAILED\033[0m against %s\n' "$CONTEXT"
  printf 'A failure here is either a real regression or genuine Kubernetes behaviour drift.\n'
  printf 'Both matter: the detectors key on reason strings and status fields, and when those\n'
  printf 'change upstream the fixtures captured on an older version stop being evidence.\n'
fi
exit "$fail"
