# Running kestci in CI

`kestci` is the non-interactive sibling of `kest swoop`: same discovery and the
same resolution spine (`internal/swoop`), but a deliberately constrained
surface meant to read clearly in a workflow file. This guide covers where it
fits, how it authenticates, and a few ready-to-adapt workflows under
[`examples/ci/`](examples/ci/).

## Match CI to the blast radius

The most important decision is *not* which CI provider — it's **which stacks
belong in CI at all.** The rule: the thing that manages a system must sit
outside that system's blast radius. Tier your roots and place the management
plane accordingly.

| Tier | Example stacks | A bad apply takes down… | Manage from |
| --- | --- | --- | --- |
| 0 — foundation | `cluster-base` | everything | **workstation only** (`kest swoop`) |
| 1 — cluster | `cluster`, `cluster-config` | the cluster, incl. any in-cluster runner | **workstation only** |
| 2 — platform | `k8s-platform`, `authentik` | maybe your CI/SSO themselves | workstation; in-cluster CI only with eyes open |
| 3 — app / edge | `cloudflare`, `ses-smtp` | nothing you depend on to recover | hosted CI is safe |

Consequences:

- **Tier 0/1 should not run in CI at all.** A runner that lives in the cluster
  it deploys is a circular dependency: break the cluster and you've lost the
  tool that fixes it, with no out-of-band path back. Keep these on
  `kest swoop apply` from a workstation that depends on none of it.
- **Tier 3 is the sweet spot for hosted CI** (GitHub/Gitea cloud): public-API
  providers, no LAN access, no circular dependency, no LAN trust surface.
- Because `kest` and `kestci` share a resolution spine, **CI is never a hard
  dependency.** Whatever you automate, `kest swoop apply` from your laptop
  reproduces it byte-for-byte — your break-glass path, as long as you keep it
  exercised.

LAN-bound stacks (Tier 1/2) that you *do* want automated need the runner on the
network — a self-hosted runner, or a hosted runner joined to the LAN via
Tailscale (subnet router) / Cloudflare Tunnel. That's an orthogonal networking
layer; `kestci` itself doesn't care how packets reach `192.168.x.x`.

## Credentials

`kestci` runs under an **ambient** credential policy: it never sets
`AWS_PROFILE`. Credentials come from the environment, and `kestci` only
*resolves and echoes* the account/profile it expects, so a mismatch fails
legibly. Supply them per layer:

- **AWS (state backend):** prefer GitHub OIDC →
  `aws-actions/configure-aws-credentials` assuming a role whose trust policy is
  scoped to the repo and branch. That trust policy — not `kestci` — is the
  authority boundary; `kestci` deliberately does not gate branches. The roots'
  own `backend "s3"` `assume_role` then chains from those ambient creds.
  (Gitea has no turnkey OIDC→AWS federation; there you'll typically hand the
  runner a scoped static IAM key instead — see the Gitea note below.)
- **Providers (Cloudflare, Authentik, …):** repo/environment secrets exported
  as the env vars the provider blocks read (`CLOUDFLARE_API_TOKEN`, etc.).
- **`CI=true`** is set automatically by GitHub/Gitea Actions, satisfying
  `kestci`'s CI-only guard.

## Command surface (what CI can rely on)

`kestci init|plan|apply [target]` selects roots the same way `kest swoop` does.
The target is an exact path, a glob (`authentik/*`), or a substring that
matches exactly one root. A substring matching several roots is an error, and
fuzzy matches are refused. `--dir <top-level dir>` and `--changed[=<ref>]`
narrow or replace the target. A glob, `--dir` or `--changed` that matches
several roots runs them in sequence and ends with a per-root summary. `apply`
always enforces a clean worktree; there is no `--force` in CI.

`--changed` picks roots whose `.tf`, `.tf.json`, `.tfvars`, lock file or
version pin changed since the merge-base with `main`/`master` (or
`origin/main`/`origin/master`), or since the ref you pass. It needs that
history, so check out with `fetch-depth: 0`, or pass a ref the checkout has.
If nothing changed, kestci prints so and exits 0. Edits in local modules
outside a root don't select it.

Version pins are checked, never written or installed. If the active
`terraform`/`tofu` doesn't match a root's `.opentofu-version` /
`.terraform-version`, `init` and `plan` warn and `apply` refuses, since applying
with another version restamps state. Install the pinned version in the
workflow, or move the pin with `kest swoop pin`.

Discovery honors `.kestconfig` (`terraform.command`, `terraform.iac_dir`), so
commit it at the repo root.

`kestci plan <target> --out` saves the plan to `.terraform/kest.tfplan` (plus a
`.json` sidecar) and `kestci apply <target> --plan` applies exactly that file.
To apply in a later job, carry both files across as an artifact. The apply job
must check out the same commit, or kest refuses the plan as stale.
`--summary` prints one line per changed resource, which fits in a PR comment.

## Patterns

- **PR plan → apply on merge** — [`examples/ci/github-plan-apply.yml`](examples/ci/github-plan-apply.yml).
  Plan every PR (read-only), apply on push to `master` behind an optional
  protected Environment.
- **Scheduled drift detection** — [`examples/ci/github-drift-detection.yml`](examples/ci/github-drift-detection.yml).
  Cron a plan and alert when a root no longer matches its config.

Both fan out one job per root via a matrix, which keeps each root's logs and
status separate. `kestci plan --changed` in a single job is the simpler
alternative for PR plans. Keep the matrix list scoped to Tier-3 stacks.

## Current limitations (CI ergonomics)

These are known gaps where `kestci` lags `kest swoop`; they shape the examples
above and are tracked in [roadmap.md](roadmap.md):

- **No terraform-flag passthrough.** Can't pass `-detailed-exitcode`,
  `-lock-timeout`, `-var`, etc. Drift detection therefore parses plan output
  for "No changes" rather than reading an exit code.
- **State is local-only.** `kestci` records plan/apply timestamps to swoop
  state, which is ephemeral on a runner — don't rely on it across jobs. For PR
  feedback, post `kestci plan --summary` output as a comment.

## A note on self-hosted / Gitea

Self-hosting the runner (e.g. Gitea Actions, GitHub-Actions-compatible) removes
the LAN problem for Tier-2 stacks and keeps everything on your own
infrastructure — but it reintroduces the blast-radius trap above for any Tier
0/1 stack, and makes the runner a credential- and network-bearing surface you
now have to secure and keep patched. Use it for app-tier convenience if you
like; never for the foundation. The workstation + shared spine remains the
fallback either way.
