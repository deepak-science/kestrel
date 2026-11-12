# Roadmap / backlog

Ideas captured but not scoped for the next release. Order is rough priority, not commitment.

## Applier ergonomics (field feedback from a multi-app upgrade run)

A ~30-change upgrade pass (app image bumps, helm chart majors, platform TF roots) driven largely by an agent through `kest deploy` and `kest swoop`. kest was a correct applier throughout: no wrong-context or wrong-root applies, substring root matching always hit the intended root. All friction was at the edges — waiting, verifying, and proving that what was reviewed is what ran.

### P0 — small, unblocks non-interactive use

- **SSO fail-fast without a TTY — DONE.** With `auto_sso_login` on and no TTY (agent, pipe, cron), `awslogin.EnsureSession` used to open a browser flow and block forever. It now returns the `aws sso login --profile <p>` command to run and exits non-zero.
- **`kest deploy --wait` — DONE.** Manifest deploys run `rollout status` on every Deployment/StatefulSet/DaemonSet after apply and print each workload's images (`--wait-timeout`, default 5m). On by default; `--wait=false` skips it.

### P1 — reviewed == applied

- **Saved-plan apply for swoop — DONE.** `swoop plan --out` writes `.terraform/kest.tfplan` plus a sidecar recording git HEAD, a hash of the root's `.tf`/`.tfvars`/lock files, and the AWS profile. `swoop apply --plan` refuses if the planfile is missing or any of those changed, prints the summary, then applies without a prompt; the planfile is removed after a successful apply. Terraform itself rejects plans whose state has moved. Same flags on `kestci`. A bare `--auto-approve` stays unsupported. Open: local modules outside the root are covered only by HEAD + the clean-worktree guard, so a `--force` apply with dirty module edits isn't caught (terraform still applies the reviewed plan, just not those edits).
- **`--summary` for swoop plan — DONE.** Renders `show -json` as one line per changed resource with changed attribute paths, masking sensitive values; implies `--out`. Open: feed it into `PlanSummary`/state instead of the parsed plan line.
- **`kest deploy --confirm` — DONE.** Diff, prompt, apply in one invocation (`--diff --confirm` is the same). A manifest diff with no differences skips the apply; helm always asks. For helm this is "same invocation", not atomic. kest only; kestci has no prompt.

### P1 — manifest apply fidelity

- **Server-side apply for manifest deploys.** Client-side `kubectl apply` has two observed problems:
  - No-op objects print `configured` on every apply (e.g. a PersistentVolume with defaulted fields) though `kubectl diff` is empty, which hides real drift.
  - It only prunes fields recorded in `last-applied`, so a field set by a live `kubectl patch` survives a re-apply of the manifest. A "reconcile back to git" deploy silently left a patched `nodeSelector` in place.
  
  **DONE as opt-in `server_side: true`** (plus `--force-conflicts`). Verified on a live cluster, some of the original assumptions were wrong:
  - kubectl prints `serverside-applied` for every object, changed or not. kest instead derives created/changed/unchanged from `resourceVersion` before vs after, since a no-op apply doesn't bump it, and prints "N created, M changed, K unchanged".
  - SSA does **not** fix the patched-`nodeSelector` case. A field set by another manager that the manifest never mentions survives SSA too, and `diff --server-side` doesn't show it. SSA does fail loudly with a conflict when someone live-edits a field kest manages; `--force-conflicts` reverts it.
  - CSA → SSA migration was a clean no-op on objects last applied client-side.

  **Now the default** for manifest deploys (`server_side: false` opts out). A server-side diff across 15 real manifest deploys found one conflict: a StatefulSet's `volumeClaimTemplates`, an atomic field that kubectl doesn't hand from client-side to server-side ownership once defaults are filled in. kest now explains that error and points at a one-time `--force-conflicts`.
  Open: detecting foreign-manager fields would need a `managedFields` audit, e.g. a `--drift` report listing fields owned by managers other than `kest` and known controllers.
- **Prune deleted manifests.** Removing a file from a manifest dir leaves the object running; it had to be deleted by hand. `kubectl apply --prune` is legacy and dangerous; ApplySets (`--applyset`, `KUBECTL_APPLYSET=true`) are the modern path. Opt-in per deploy entry (`prune: true`), always shown in `--diff`. Higher risk, so after SSA lands.

### P2 — scoping and rituals

- **`kest deploy <app> --only <name>`.** A manifest dir that bundles several apps is one deploy unit, so bumping one re-applies all. File/label filter to shrink blast radius. Lower value once SSA makes no-ops truly no-op.
- **`kest snapshot <app>` — DONE.** Uses generic CSI `VolumeSnapshot`, not storage-specific CRDs. It snapshots every bound PVC in the deploy's namespace, picking a class per storage driver (or `snapshot_class:`), waits for `readyToUse`, and surfaces `status.error` on timeout. It's all or nothing: an unsnapshottable bound PVC fails the run. `deploy --snapshot` runs it before deploy scripts and the apply, on kest and kestci. Open:
  - Retention: snapshots are labeled `kest/deploy=<app>` but never pruned. A `--keep N` would cover it.
  - Restore stays manual (PVC `dataSource`).
- **`snapshot_skip:` — DONE.** Per-deploy globs on PVC name or `namespace/name` leave volumes out of snapshots; each skipped PVC is printed, and the all-or-nothing error points at it.

## Small fixes shipped alongside

- **Version-pin warnings name a fix.** A root's pin is rewritten with `kest swoop pin [target] [--version V]`, which defaults to the active binary's version and to every root when no target is given. The mismatch warning suggests `swoop pin` when the active binary is newer than the pin, or installing the pinned version otherwise, instead of "install manually". Open: `kestci` still doesn't check the pin at all (see the parity section).
- **Runtime errors no longer print usage.** Usage text is kept for argument errors only.

## Per-guard config flags (replace blanket `--force`)

Today `--force` (`cmd/root.go`) bypasses all three guards in `internal/guard/guard.go` at once: `CheckCI`, `CheckCleanWorktree`, `CheckBranch`. That's coarse — quick local applies from a laptop only really need the CI guard off, not the dirty-worktree or branch ones.

Proposed shape (global config only, never project config — must not be committable):

```yaml
guards:
  require_ci: false              # default true
  require_clean_worktree: true   # default true
  require_main_for_prod: true    # default true
```

Notes:
- Each guard consults config before erroring; `--force` stays as a master override on the CLI.
- `kest doctor` should surface a loud warning whenever any of these are disabled, so a misconfigured machine is visible.
- Project-level `.kestconfig` must not be allowed to flip these — otherwise a repo could ship "no CI required" to everyone.

## `swoop list` AWS column should reflect what apply actually uses

`printRootTable` (`cmd/swoop.go:233`) fills the AWS column from `resolve.AWSProfileForRoot(cfg, r.Dir, r.AccountID, environment)` alone. That returns empty unless the root has an `allowed_account_ids`-style marker, a `directories` mapping, or an active `-e` target — so for repos whose only AWS touchpoint is an S3 backend with `assume_role` (e.g. the homelab repo, account `111122223333` → `homelab`), every root shows `-`.

But the *executor* resolves more: `cmd/swoop_actions.go` adds a `backendProfileFor` fallback (`swoop.ExtractBackendAuth`, reading the backend `role_arn`) at lines ~216/229. So `kest swoop apply` runs with `AWS_PROFILE=homelab` while `kest swoop list` claims `-`. The list lies about what apply does.

Proposed shape:
- Factor the executor's "profile + backend fallback" into one resolver (e.g. `resolve.EffectiveProfileForRoot`) and call it from both `printRootTable` and the execute path, so they can't drift.
- Consider marking backend-only resolution distinctly (e.g. `homelab (state)`) since it's the state/backend profile, not necessarily a resource-provider profile.

Notes:
- Pure correctness/clarity fix; no behavior change to apply itself.
- Same divergence exists between local (`cmd/swoop_actions.go`) and CI (`cmd/kestci/terraform.go`) — CI does *not* apply the backend fallback at all (relies on ambient creds). A shared resolver closes both gaps at once. See the local/CI parity note below.

## Project-config bootstrap (`kest config init`)

`kest config autoconfigure` (`cmd/config_autoconfigure.go`) only ever writes `config.GlobalConfigPath()` — it discovers AWS profiles and kube contexts and records them globally. It *reads* an existing `.kestconfig` to match contexts to targets, but never scaffolds one. So every project's `.kestconfig` (command, iac_dir, targets, releases) is hand-authored today, which is the main reason per-project config goes unused.

Almost all of a terraform-side `.kestconfig` is mechanically derivable:

```yaml
terraform:
  command: tofu       # detect: .opentofu-version / tofu in provider lock / which binary
  iac_dir: iac-live   # the dir under which swoop.Discover finds the most backend-bearing roots
```

Proposed shape: a `kest config init` (or `autoconfigure --project`) that:
- Detects `tofu` vs `terraform` and writes `command`.
- Runs `swoop.Discover` from the repo root, finds where backend-bearing roots cluster, and proposes `iac_dir`.
- Optionally scaffolds `targets:` stubs from discovered kube contexts (commented, since binding cluster+account+region needs a human).
- Writes to `./.kestconfig`, never the global file; previews + editor-confirm like autoconfigure already does.

Notes:
- Helm releases stay manual — they encode intent (which release → which target) that can't be inferred.
- This is the capability behind the autoconfigure hint text; the hint narrates the manual flow, this replaces it.

## Shared resolve/execute spine for local and CI (parity)

`kest swoop apply` (`cmd/swoop_actions.go:executeSingle`) and `kestci apply` (`cmd/kestci/terraform.go:executeCIAction`) are parallel reimplementations of the same discover → resolve → execute → record-state loop. They're *supposed* to differ only in interactivity (fuzzy matching, SSO prompts) and credential source (named profile vs ambient). In practice they've also drifted where they shouldn't have:

- **Credentials:** local adds the `backendProfileFor` / `ExtractBackendAuth` fallback (resolves `homelab` from the backend `assume_role`); CI does not. A root that `kest swoop apply` runs cleanly can fail in CI with a state-backend auth error unless the runner happens to have ambient creds that can assume the role.
- **Versions:** local does `EnsureTFVersion` + `handleTFVersionCheck` (write/install/verify the pin); CI skips all of it and runs whatever `tofu`/`terraform` is on PATH. The official CI image papers over this by pinning its own binary (`KEST_TERRAFORM_VERSION_MANAGER=off`), but outside that image the executed version can differ from local.

So "works on my machine via swoop" does not currently predict "works in `kestci`" — which undercuts the whole point of a shared `.kestconfig`.

Proposed shape: a single resolver+executor that both command surfaces call, parameterized by a small policy struct:

```
type ExecPolicy struct {
  AllowFuzzy      bool   // kest: true, kestci: false
  CredentialMode  enum   // Profile (kest) | Ambient (kestci)
  InteractiveSSO  bool   // kest: true, kestci: false
}
```

Resolution of root → profile (incl. backend fallback), version pin, and state recording live in one place; only the policy flags differ. Interactivity and fuzzy stay local-only by policy, deliberately — not by accident of which file the loop was written in.

Notes:
- Closes the `swoop list` column divergence above for free (same resolver feeds the table).
- **Two binaries is deliberate, not incidental.** A constrained `kestci` surface (no fuzzy, no SSO prompts, explicit targets only, ambient creds) *communicates intent* in a workflow file — it's a feature, not just a smaller image. The fix is to keep the constrained front end while sharing the spine, so "constrained" never means "separately maintained and drifting."
- `kestci` has been allowed to stagnate while features landed on `kest`; the spine is what lets CI catch up without re-porting each feature by hand.

## kestci CI design (AWS-centric, the original intent)

The homelab is the atypical consumer of kest (non-AWS providers, LAN-only, non-EKS). The original and primary problem space is AWS-centric CI — work repos deploying to multiple EKS clusters across accounts. That's where `kestci` should be strongest, and it's the case that most needs design now that the spine work (above) makes parity achievable.

**Decided — authority lives in OIDC/IAM, not in kest:**
- **Role assumption:** `kestci` does *not* own "environment → account → assume role." The OIDC action assumes the role before `kestci` runs; `kestci` only *resolves and echoes* the account/role it expects, so a credential mismatch fails legibly instead of cryptically. Credentials stay ambient.
- **Branch/environment gating:** not built into kest. The OIDC trust policy guards it — it validates GitHub claims (environment, ref) and simply won't mint prod credentials outside the right context. Making the wrong assume-role *impossible* beats a bypassable `guard.CheckBranch`, so the CLI doesn't duplicate (or weaken) that boundary. The existing clean-worktree guard stays; branch gating does not move into kest.

**Still open:**
- **Plan surfacing:** partly addressed — `kestci plan --summary` emits a compact summary to stdout and `--out` leaves a planfile to carry as an artifact. The example workflows don't use them yet.
- **Self-hosted runners** are on the table (homelab LAN, or work-specific networks). A first step can be `kestci` commands wrapped in a `justfile` before any GitHub Actions wiring — the binary should be pleasant to drive that way too.
- **`--changed` on kestci.** `kest swoop` scopes to roots a branch touched; `kestci` can't, so CI workflows fan out an explicit matrix instead (see [ci.md](ci.md) / [examples/ci/](examples/ci/)). Porting `--changed` (and `--dir`) onto kestci would let `kestci plan --changed` gate PRs directly.
- **Terraform-flag passthrough.** kestci `init|plan|apply` take exactly one target and forward nothing, so CI can't pass `-detailed-exitcode` (drift detection parses "No changes" out of plan output instead), `-lock-timeout`, `-var`, etc. A trailing `-- <args>` passthrough would fix the drift-exit-code case cleanly.

These two are the concrete CI-ergonomics gaps the [ci.md](ci.md) examples work around today.

## Deploy routines (helm + manifests) — DONE

Shipped as `kest deploy` / `kestci deploy`: a cluster-agnostic app-deploy spine
(`internal/deploy`) with a pluggable executor (helm chart or raw manifests,
chosen per `deploys:` entry), shared target resolution, guard stack, and
ambient-vs-profile policy. Supports local in-repo charts, third-party/OCI charts,
and `kubectl apply -f` manifest dirs; `--diff` is the plan analog. Targets gained
an optional explicit `kubeconfig`, and cluster names resolve to named contexts
(a bare named context like `my-cluster`) without an EKS ARN. Design + rationale:
[deploy-routines.md](deploy-routines.md); user guide: [deploy.md](deploy.md);
example: [examples/deploy/](../examples/deploy/).

Deliberately left for later:
- **`kestci deploy` for EKS manifests** would want a step that writes a
  kubeconfig for the cluster before deploy (today: supply `KUBECONFIG` or a
  target `kubeconfig:`).
- **`kustomize` executor** (`kubectl apply -k`) if a repo ever needs it.

