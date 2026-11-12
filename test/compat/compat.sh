#!/usr/bin/env bash
#
# Terraform / OpenTofu compatibility smoke tests for kest.
#
# Exercises the binary-override surface end-to-end: command resolution from
# config + $KEST_TERRAFORM_COMMAND, version-manager probing in `kest doctor`,
# and the actual terraform/tofu invocation through `kest -e <target> terraform`.
#
# Unit tests cover the config resolver. This script covers the path from a
# real .kestconfig through to a real terraform/tofu binary on PATH.
#
# Usage:
#   test/compat/compat.sh                 # builds via `go build`
#   test/compat/compat.sh /path/to/kest   # uses an existing binary
#   KEST=./result/bin/kest test/compat/compat.sh
#
# Requires `terraform` and `opentofu` (tofu) on PATH.

set -euo pipefail

KEST="${1:-${KEST:-}}"
KESTCI="${KESTCI:-}"
if [[ -z $KEST ]]; then
  echo "building kest + kestci via go build..."
  BIN="$(mktemp -d)"
  go build -o "$BIN/kest" . && go build -o "$BIN/kestci" ./cmd/kestci
  KEST="$BIN/kest"
  KESTCI="$BIN/kestci"
fi
[[ -x $KEST ]] || {
  echo "kest binary not found or not executable: $KEST" >&2
  exit 2
}

command -v terraform >/dev/null || {
  echo "terraform not on PATH" >&2
  exit 2
}
command -v tofu >/dev/null || {
  echo "tofu not on PATH" >&2
  exit 2
}

# ── tiny assertion framework ───────────────────────────────────────────────

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

assert_contains() {
  # assert_contains <name> <needle> <haystack>
  if [[ $3 == *"$2"* ]]; then ok "$1"; else fail "$1" "*$2*" "$3"; fi
}

assert_not_contains() {
  if [[ $3 != *"$2"* ]]; then ok "$1"; else fail "$1" "NOT *$2*" "$3"; fi
}

# ── project fixture ────────────────────────────────────────────────────────

# An isolated project root with a tiny terraform module under iac/live/dev/.
# A backend "local" {} block keeps things hermetic (no network, no S3).
mkproject() {
  local dir
  dir="$(mktemp -d)"
  mkdir -p "$dir/iac/live/dev"
  cat >"$dir/iac/live/dev/main.tf" <<'EOF'
terraform {
  backend "local" {}
}
EOF
  cat >"$dir/.kestconfig" <<'EOF'
terraform:
  iac_dir: iac
targets:
  dev:
    cluster: ""
EOF
  printf "%s" "$dir"
}

# write_kestconfig <project> <extra-terraform-yaml>
# Replaces .kestconfig with terraform block including the extra yaml.
write_kestconfig() {
  local proj=$1
  local extra=$2
  cat >"$proj/.kestconfig" <<EOF
terraform:
  iac_dir: iac
$extra
targets:
  dev:
    cluster: ""
EOF
}

# ── tests ──────────────────────────────────────────────────────────────────

# Isolate state: HOME points at a tmp so we don't read/write real ~/.config.
HOME="$(mktemp -d)"
export HOME

echo
echo "▶ doctor: command override"

# Doctor with default config → invokes terraform and parses its version.
# The Tools section labels the configured command; the detail looks like
# "v1.14.0" (doctor strips the "Terraform v" / "OpenTofu v" prefix).
out=$("$KEST" doctor 2>&1 || true)
assert_contains "doctor default lists 'terraform' tool" "terraform" "$out"
assert_not_contains "doctor default parsed a version (no '(version unknown)')" "(version unknown)" "$out"

# With KEST_TERRAFORM_COMMAND=tofu the resolved label flips to 'tofu' and
# OpenTofu's `version` output must be parseable by the same regex.
out=$(KEST_TERRAFORM_COMMAND=tofu "$KEST" doctor 2>&1 || true)
assert_contains "doctor env-override lists 'tofu' tool" "tofu" "$out"
assert_not_contains "doctor env-override parsed a version (no '(version unknown)')" "(version unknown)" "$out"

echo
echo "▶ doctor: version_manager probe"

# tfenv isn't packaged in nixpkgs, so doctor must warn "not found" — which
# proves the manager line is being rendered.
out=$("$KEST" doctor 2>&1 || true)
assert_contains "doctor reports tfenv not found" "tfenv" "$out"
assert_contains "doctor tfenv warning text" "not found" "$out"

# $KEST_TERRAFORM_COMMAND=tofu must also flip the manager auto-detect to
# tofuenv (otherwise tfenv users on tofu would get the wrong probe).
out=$(KEST_TERRAFORM_COMMAND=tofu "$KEST" doctor 2>&1 || true)
assert_contains "doctor reports tofuenv when command=tofu" "tofuenv" "$out"

# With version_manager: off in config, doctor must NOT list any manager.
proj=$(mkproject)
write_kestconfig "$proj" "  version_manager: off"
out=$(cd "$proj" && "$KEST" doctor 2>&1 || true)
assert_not_contains "doctor omits manager when off (no tfenv line)" "tfenv" "$out"
assert_not_contains "doctor omits manager when off (no tofuenv line)" "tofuenv" "$out"

echo
echo "▶ swoop init: kest invokes the configured command end-to-end"

# A minimal `terraform { backend "local" {} }` root is enough for
# `terraform init` / `tofu init` to succeed without network. The success
# banner contains "Terraform" or "OpenTofu" verbatim, which is what we
# assert on to prove the right binary actually ran.

proj=$(mkproject)
out=$(cd "$proj" && "$KEST" swoop init live/dev 2>&1 || true)
assert_contains "default swoop init runs terraform" "Terraform" "$out"
assert_not_contains "default swoop init did NOT run tofu" "OpenTofu" "$out"

proj=$(mkproject)
out=$(cd "$proj" && KEST_TERRAFORM_COMMAND=tofu "$KEST" swoop init live/dev 2>&1 || true)
assert_contains "env-override swoop init runs tofu" "OpenTofu" "$out"

proj=$(mkproject)
write_kestconfig "$proj" "  command: tofu"
out=$(cd "$proj" && "$KEST" swoop init live/dev 2>&1 || true)
assert_contains "project config command=tofu runs tofu" "OpenTofu" "$out"

# $KEST_TERRAFORM_COMMAND must beat project config.
out=$(cd "$proj" && KEST_TERRAFORM_COMMAND=terraform "$KEST" swoop init live/dev 2>&1 || true)
assert_contains "env beats project config (runs terraform)" "Terraform" "$out"
assert_not_contains "env beats project config (no OpenTofu)" "OpenTofu" "$out"

echo
echo "▶ saved plans: plan --summary / apply --plan"

# terraform_data is built in, so this plans and applies with no provider download.
for tf in terraform tofu; do
  proj=$(mkproject)
  cat >>"$proj/iac/live/dev/main.tf" <<'EOF'
variable "secret" {
  default   = "hunter2"
  sensitive = true
}
resource "terraform_data" "app" {
  input = { version = "1.0.0", token = var.secret }
}
EOF
  export KEST_TERRAFORM_COMMAND=$tf
  (cd "$proj" && "$KEST" swoop init live/dev >/dev/null 2>&1)

  out=$(cd "$proj" && "$KEST" swoop apply live/dev --plan --force 2>&1 || true)
  assert_contains "$tf: apply --plan without a plan refuses" "no saved plan" "$out"

  out=$(cd "$proj" && "$KEST" swoop plan live/dev --summary 2>/dev/null || true)
  assert_contains "$tf: summary lists the resource" "+   terraform_data.app" "$out"
  assert_contains "$tf: summary counts" "Plan: 1 to add, 0 to change, 0 to destroy." "$out"
  assert_not_contains "$tf: summary omits full plan output" "will be created" "$out"

  sed -i 's/1.0.0/2.0.0/' "$proj/iac/live/dev/main.tf"
  out=$(cd "$proj" && "$KEST" swoop apply live/dev --plan --force 2>&1 || true)
  assert_contains "$tf: config edit after plan is stale" "root config changed since the plan" "$out"

  (cd "$proj" && "$KEST" swoop plan live/dev --out >/dev/null 2>&1)
  out=$(cd "$proj" && "$KEST" swoop apply live/dev --plan --force 2>&1 || true)
  assert_contains "$tf: apply --plan applies without prompting" "1 added" "$out"
  if [[ -e $proj/iac/live/dev/.terraform/kest.tfplan ]]; then
    fail "$tf: applied plan is removed" "no planfile" "planfile present"
  else
    ok "$tf: applied plan is removed"
  fi

  sed -i 's/2.0.0/3.0.0/' "$proj/iac/live/dev/main.tf"
  out=$(cd "$proj" && "$KEST" swoop plan live/dev --summary 2>/dev/null || true)
  assert_contains "$tf: update shows changed attribute" 'input.version: "2.0.0" → "3.0.0"' "$out"
  assert_not_contains "$tf: summary masks sensitive values" "hunter2" "$out"
  unset KEST_TERRAFORM_COMMAND
done

# ── kestci: shares root selection and the batch runner with kest swoop ──────
if [[ -n $KESTCI && -x $KESTCI ]]; then
  echo
  echo "▶ kestci: selection, batch summary, version pin, --changed"
  export KEST_TERRAFORM_COMMAND=tofu
  proj=$(mkproject)
  mkdir -p "$proj/iac/live/dev2"
  cp "$proj/iac/live/dev/main.tf" "$proj/iac/live/dev2/main.tf"
  printf '.terraform/\n.terraform.lock.hcl\n' >"$proj/.gitignore"
  g() { git -C "$proj" -c user.email=ci@example.com -c user.name=ci -c commit.gpgsign=false "$@"; }
  g init -q
  g add -A
  g commit -qm init
  ci() { (cd "$proj" && CI=true "$KESTCI" "$@" 2>&1 || true); }

  out=$(ci plan dev)
  assert_contains "kestci: substring matching two roots is ambiguous" 'target "dev" matches 2 roots' "$out"
  out=$(ci plan)
  assert_contains "kestci: no target and no filter is refused" "target required" "$out"

  out=$(ci init 'live/*')
  assert_contains "kestci: glob runs a batch" "Running terraform init on 2 root(s)" "$out"
  assert_contains "kestci: batch prints the shared summary" "Swoop init summary" "$out"

  out=$(ci plan --changed=HEAD)
  assert_contains "kestci: --changed with nothing changed succeeds" "no roots changed; nothing to do" "$out"

  echo "1.0.0" >"$proj/iac/live/dev/.opentofu-version"
  g add -A
  g commit -qm pin
  out=$(ci apply live/dev)
  assert_contains "kestci: apply refuses a version that doesn't match the pin" "refusing to apply with a different version" "$out"
  out=$(ci plan live/dev)
  assert_contains "kestci: plan only warns on a pin mismatch" "live/dev pins tofu 1.0.0" "$out"
  assert_contains "kestci: plan still runs after the warning" "No changes" "$out"

  echo "# touched" >>"$proj/iac/live/dev2/main.tf"
  out=$(ci plan --changed=HEAD)
  assert_contains "kestci: --changed selects the edited root" "root:    live/dev2" "$out"
  assert_not_contains "kestci: --changed skips untouched roots" "root:    live/dev " "$out"
  unset KEST_TERRAFORM_COMMAND
fi

# ── summary ────────────────────────────────────────────────────────────────

echo
printf "%d passed, %d failed\n" "$pass" "$fail"
if ((fail > 0)); then
  printf "failed: %s\n" "${fail_names[*]}"
  exit 1
fi
