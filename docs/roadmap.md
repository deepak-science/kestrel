# Roadmap / backlog

Ideas captured but not scoped for the next release. Order is rough priority, not commitment.

## Applier ergonomics (field feedback from a multi-app upgrade run)

A ~30-change upgrade pass (app image bumps, helm chart majors, platform TF roots) driven largely by an agent through `kest deploy` and `kest swoop`. kest was a correct applier throughout: no wrong-context or wrong-root applies, substring root matching always hit the intended root. All friction was at the edges — waiting, verifying, and proving that what was reviewed is what ran.

### P0 — small, unblocks non-interactive use

- **SSO fail-fast without a TTY — DONE.** With `auto_sso_login` on and no TTY (agent, pipe, cron), `awslogin.EnsureSession` used to open a browser flow and block forever. It now returns the `aws sso login --profile <p>` command to run and exits non-zero.
- **`kest deploy --wait`.** Every deploy needed a hand-run `kubectl rollout status` per workload. Helm already waits via `--atomic`; manifest deploys don't. Want `--wait[=timeout]`: after `kubectl apply`, run `rollout status` for every Deployment/StatefulSet/DaemonSet in the applied set, and print the resulting image per workload. Candidate default-on for manifest deploys.

### P1 — reviewed == applied

- **Saved-plan apply for swoop.** `swoop apply` re-plans, so the reviewed plan isn't guaranteed to be the one that runs. Want `kest swoop plan <root> --out` (writes a planfile under the root, e.g. `.terraform/kest.tfplan`) and `kest swoop apply <root> --plan` (applies that file, refusing if missing/stale). Terraform applies a saved plan without prompting, which gives a safe `--auto-approve` for free; a bare `--auto-approve` without a planfile stays disallowed. Today the workaround is `echo yes |`.
- **`--summary` for swoop plan.** `tofu show -json <planfile>` → print only resource addresses, action, and changed attribute paths (e.g. `~ helm_release.traefik: version 41.4.0 → 42.3.1`). Replaces grepping ANSI-stripped plan output. Builds on the planfile above; could also feed `PlanSummary` and the kestci plan-surfacing gap.
- **`kest deploy --diff --confirm`.** Show diff, prompt, apply in one invocation instead of two. For helm, `helm diff` isn't atomic with upgrade either, so this is "same invocation", not a hard guarantee.

### P1 — manifest apply fidelity

- **Server-side apply for manifest deploys.** Client-side `kubectl apply` has two observed problems:
  - No-op objects print `configured` on every apply (e.g. a PersistentVolume with defaulted fields) though `kubectl diff` is empty, which hides real drift.
  - It only prunes fields recorded in `last-applied`, so a field set by a live `kubectl patch` survives a re-apply of the manifest. A "reconcile back to git" deploy silently left a patched `nodeSelector` in place.
  
  `kubectl apply --server-side --field-manager=kest` (with `--force-conflicts` behind a flag) fixes both: unchanged objects report `unchanged`, and kest owns its fields. Then summarize output as "N changed, M unchanged". Needs a one-time migration note: the first SSA run over CSA-managed objects takes over `last-applied` fields, which kubectl handles, but a conflicting manager (e.g. a field owned by a controller) needs `--force-conflicts`.
- **Prune deleted manifests.** Removing a file from a manifest dir leaves the object running; it had to be deleted by hand. `kubectl apply --prune` is legacy and dangerous; ApplySets (`--applyset`, `KUBECTL_APPLYSET=true`) are the modern path. Opt-in per deploy entry (`prune: true`), always shown in `--diff`. Higher risk, so after SSA lands.

### P2 — scoping and rituals

- **`kest deploy <app> --only <name>`.** A manifest dir that bundles several apps is one deploy unit, so bumping one re-applies all. File/label filter to shrink blast radius. Lower value once SSA makes no-ops truly no-op.
- **`kest snapshot <app>`.** Every upgrade started with hand-written snapshot CRs (PVC → volume → CR → wait ready). Generic shape: create a CSI `VolumeSnapshot` for every PVC in the deploy's namespace and wait for `readyToUse`; `deploy --snapshot` hangs it off the deploy. Open question: CSI VolumeSnapshot vs storage-specific CRDs — CSI keeps kest cluster-agnostic, but requires a snapshot class to be configured.

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
- **Plan surfacing:** `PlanSummary` is recorded to local state, which is ephemeral on a runner. PR-comment / plan-artifact workflows need plan output emitted to stdout/file, not just state.
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

