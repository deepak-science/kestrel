#!/usr/bin/env bash
#
# Smoke tests for the `kest deploy` / `kestci deploy` manifest+helm spine.
#
# Exercises the path from a real .kestconfig through `kest deploy` to a real
# (stubbed) kubectl/helm invocation: executor selection (chart vs manifests),
# cluster-agnostic context resolution, explicit kubeconfig, --diff, third-party
# chart flags, --all, and the guard posture.
#
# Unit tests (internal/deploy) cover argument construction in isolation. This
# script covers command wiring + config resolution end-to-end with no cluster:
# `kubectl` and `helm` are replaced by stubs on PATH that echo their argv.
#
# Usage:
#   test/deploy/deploy.sh                 # builds kest + kestci via `go build`
#   test/deploy/deploy.sh /path/to/kest   # uses an existing kest binary
#   KEST=./result/bin/kest test/deploy/deploy.sh
#
# Requires only a Go toolchain (or prebuilt binaries) + bash. No cluster.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

KEST="${1:-${KEST:-}}"
KESTCI="${KESTCI:-}"
if [[ -z $KEST ]]; then
  echo "building kest + kestci via go build..."
  BIN="$(mktemp -d)"
  (cd "$REPO_ROOT" && go build -o "$BIN/kest" . && go build -o "$BIN/kestci" ./cmd/kestci)
  KEST="$BIN/kest"
  KESTCI="$BIN/kestci"
fi
[[ -x $KEST ]] || {
  echo "kest binary not found or not executable: $KEST" >&2
  exit 2
}

# ── stub kubectl + helm on PATH ─────────────────────────────────────────────
# Each stub echoes a tagged line with its full argv so assertions can match.
# `kubectl diff` exits 1 (diff present) to prove kest treats that as success.
#
# The shebang uses the resolved bash path (not /usr/bin/env) so the stubs are
# exec'able inside the Nix build sandbox, which has no /usr/bin/env.

STUBDIR="$(mktemp -d)"
BASH_BIN="$(command -v bash)"

printf '#!%s\n' "$BASH_BIN" >"$STUBDIR/kubectl"
# With `-o json`, argv goes to stderr and stdout carries JSON: an empty cluster for get, one created object for apply.
cat >>"$STUBDIR/kubectl" <<'EOF'
if [[ " $* " == *" -o json "* ]]; then
  echo "KUBECTL $*" >&2
  case ${1:-} in
  get) echo '{"kind":"List","items":[]}' ;;
  apply) echo '{"kind":"List","items":[{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"gitea","resourceVersion":"7"}}]}' ;;
  esac
  exit 0
fi
echo "KUBECTL $*"
if [[ ${1:-} == diff ]]; then exit 1; fi
exit 0
EOF

printf '#!%s\n' "$BASH_BIN" >"$STUBDIR/helm"
cat >>"$STUBDIR/helm" <<'EOF'
echo "HELM $*"
exit 0
EOF

chmod +x "$STUBDIR/kubectl" "$STUBDIR/helm"
export PATH="$STUBDIR:$PATH"

# ── tiny assertion framework ────────────────────────────────────────────────

pass=0
fail=0
fail_names=()
ok() {
  printf "  ok   %s\n" "$1"
  pass=$((pass + 1))
}
fail() {
  printf "  FAIL %s\n        want: %s\n        got:  %s\n" "$1" "$2" "$3"
  fail=$((fail + 1))
  fail_names+=("$1")
}
assert_contains() { if [[ $3 == *"$2"* ]]; then ok "$1"; else fail "$1" "*$2*" "$3"; fi; }
assert_not_contains() { if [[ $3 != *"$2"* ]]; then ok "$1"; else fail "$1" "NOT *$2*" "$3"; fi; }

# ── project fixture ─────────────────────────────────────────────────────────
# A .kestconfig with one manifest deploy, two helm deploys (local + 3rd-party),
# and a target whose cluster is a bare named context with no
# kubernetes.contexts entry — so resolution must fall back to the literal name.

mkproject() {
  local dir
  dir="$(mktemp -d)"
  mkdir -p "$dir/k8s-manifests/gitea" "$dir/charts/app" "$dir/deploys"
  echo "{}" >"$dir/k8s-manifests/gitea/00-ns.yaml"
  echo "{}" >"$dir/deploys/homepage.yaml"
  echo "{}" >"$dir/deploys/authentik.yaml"
  cat >"$dir/.kestconfig" <<'EOF'
deploys:
  gitea:
    manifests: k8s-manifests/gitea
    target: homelab
  homepage:
    chart: charts/app
    values: [deploys/homepage.yaml]
    namespace: homepage
    target: homelab
  authentik:
    chart: authentik/authentik
    repo: https://charts.goauthentik.io
    version: "2024.10.1"
    values: [deploys/authentik.yaml]
    target: homelab
targets:
  homelab:
    cluster: my-cluster
EOF
  printf "%s" "$dir"
}

# Isolate global config under a throwaway HOME.
HOME="$(mktemp -d)"
export HOME

echo
echo "▶ manifest deploy → kubectl apply"
proj=$(mkproject)
out=$(cd "$proj" && "$KEST" deploy gitea --force 2>&1 || true)
assert_contains "manifest uses kubectl apply -f" "KUBECTL apply -f k8s-manifests/gitea" "$out"
assert_contains "manifest resolves bare context literally" "--context my-cluster" "$out"
assert_not_contains "manifest does not shell out to helm" "HELM " "$out"

echo
echo "▶ manifest deploy --diff → kubectl diff (exit 1 = diff, not failure)"
out=$(cd "$proj" && "$KEST" deploy gitea --diff 2>&1 || true)
assert_contains "diff uses kubectl diff" "KUBECTL diff -f k8s-manifests/gitea" "$out"

echo
echo "▶ helm deploy (local chart) → helm upgrade --install"
out=$(cd "$proj" && "$KEST" deploy homepage --force 2>&1 || true)
assert_contains "helm upgrade --install with release+chart" "HELM upgrade --install homepage charts/app" "$out"
assert_contains "helm namespace + create-namespace" "--namespace homepage --create-namespace" "$out"
assert_contains "helm kube-context resolved" "--kube-context my-cluster" "$out"
assert_contains "helm values file layered" "--values deploys/homepage.yaml" "$out"
assert_contains "helm apply gets --atomic" "--atomic" "$out"

echo
echo "▶ helm deploy (third-party repo chart) → --repo / --version"
out=$(cd "$proj" && "$KEST" deploy authentik --force 2>&1 || true)
assert_contains "third-party --repo" "--repo https://charts.goauthentik.io" "$out"
assert_contains "third-party --version" "--version 2024.10.1" "$out"

echo
echo "▶ helm deploy --diff → helm --dry-run, no --atomic"
out=$(cd "$proj" && "$KEST" deploy homepage --diff 2>&1 || true)
assert_contains "helm diff is --dry-run" "--dry-run" "$out"
assert_not_contains "helm diff drops --atomic" "--atomic" "$out"

echo
echo "▶ deploy --all → every app"
out=$(cd "$proj" && "$KEST" deploy --all --force 2>&1 || true)
assert_contains "all: manifest app ran" "KUBECTL apply -f k8s-manifests/gitea" "$out"
assert_contains "all: local chart ran" "HELM upgrade --install homepage" "$out"
assert_contains "all: third-party chart ran" "HELM upgrade --install authentik" "$out"

echo
echo "▶ extra args passthrough (after --)"
out=$(cd "$proj" && "$KEST" deploy gitea --force -- --server-side 2>&1 || true)
assert_contains "kubectl passthrough" "--server-side" "$out"

echo
echo "▶ server_side: true → kubectl apply --server-side as field manager kest"
sed -i 's|    manifests: k8s-manifests/gitea|&\n    server_side: true|' "$proj/.kestconfig"
out=$(cd "$proj" && "$KEST" deploy gitea --force 2>&1 || true)
assert_contains "server-side apply flags" "apply -f k8s-manifests/gitea --context my-cluster --server-side --field-manager=kest -o json" "$out"
assert_contains "pre-apply snapshot" "KUBECTL get -f k8s-manifests/gitea" "$out"
assert_contains "per-object change line" "namespace/gitea created" "$out"
assert_contains "change totals" "1 created, 0 changed, 0 unchanged" "$out"
assert_not_contains "no --force-conflicts by default" "--force-conflicts" "$out"
out=$(cd "$proj" && "$KEST" deploy gitea --force --force-conflicts 2>&1 || true)
assert_contains "--force-conflicts passed through" "--field-manager=kest --force-conflicts" "$out"
out=$(cd "$proj" && "$KEST" deploy gitea --diff 2>&1 || true)
assert_contains "server-side diff" "KUBECTL diff -f k8s-manifests/gitea --context my-cluster --server-side --field-manager=kest" "$out"
out=$(cd "$proj" && "$KEST" deploy homepage --force 2>&1 || true)
assert_not_contains "helm deploys ignore --server-side" "--server-side" "$out"
sed -i '/server_side: true/d' "$proj/.kestconfig"

echo
echo "▶ guard posture: local apply without --force is refused outside CI"
out=$(cd "$proj" && "$KEST" deploy gitea 2>&1 || true)
assert_contains "apply blocked without CI/--force" "not in CI" "$out"
assert_not_contains "blocked apply did not invoke kubectl" "KUBECTL apply" "$out"

# --diff must NOT be gated by the guard (read-only).
out=$(cd "$proj" && "$KEST" deploy gitea --diff 2>&1 || true)
assert_contains "diff is allowed without --force" "KUBECTL diff" "$out"

echo
echo "▶ explicit kubeconfig path on target (CI) → --kubeconfig"
proj2=$(mktemp -d)
mkdir -p "$proj2/k8s-manifests/app" "$proj2/iac-live/cluster/"
echo "{}" >"$proj2/k8s-manifests/app/00-ns.yaml"
echo "fake-kubeconfig" >"$proj2/iac-live/cluster/kubeconfig"
cat >"$proj2/.kestconfig" <<'EOF'
deploys:
  app:
    manifests: k8s-manifests/app
    target: homelab
targets:
  homelab:
    cluster: my-cluster
    kubeconfig: iac-live/cluster/kubeconfig
EOF
out=$(cd "$proj2" && "$KEST" deploy app --force 2>&1 || true)
assert_contains "explicit kubeconfig passed to kubectl" "--kubeconfig $proj2/iac-live/cluster/kubeconfig" "$out"

# ── kestci surface (optional — only if a kestci binary is available) ─────────
if [[ -n $KESTCI && -x $KESTCI ]]; then
  echo
  echo "▶ kestci deploy (ambient policy, CI guard)"
  # Outside CI it must refuse.
  out=$(cd "$proj" && "$KESTCI" deploy gitea 2>&1 || true)
  assert_contains "kestci refuses outside CI" "designed for CI" "$out"
  # With CI=true + a clean-ish check: the fixture is not a git repo, so the
  # clean-worktree guard errors. Use --diff to exercise resolution past guards.
  out=$(cd "$proj" && CI=true "$KESTCI" deploy gitea --diff 2>&1 || true)
  assert_contains "kestci diff resolves + runs kubectl diff" "KUBECTL diff -f k8s-manifests/gitea" "$out"
fi

# ── summary ──────────────────────────────────────────────────────────────────
echo
printf "%d passed, %d failed\n" "$pass" "$fail"
if ((fail > 0)); then
  printf "failed: %s\n" "${fail_names[*]}"
  exit 1
fi
