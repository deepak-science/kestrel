# Drift detection

`kest drift` checks every terraform root and every deploy against what's live,
without changing anything, and prints one table:

```
KIND    NAME     STATUS  DETAIL
root    dev/vpc  CLEAN   no changes
root    prd/rds  DRIFT   0 to add, 1 to change, 0 to destroy
deploy  grafana  DRIFT   chart 7.3.0 deployed, 8.0.0 configured
deploy  web      DRIFT   kubectl-patch on deployment.apps/web: spec.template.spec.nodeSelector.disktype

4 checked: 1 clean, 3 drifted, 0 errored
```

It exits 0 when everything is clean, 2 when anything drifted, and 1 when a
check failed, the same contract as `plan -detailed-exitcode`.

## What each check does

| | check | drift when |
|---|---|---|
| terraform root | `plan -detailed-exitcode -lock=false -input=false` | the plan has changes |
| manifest deploy | `kubectl diff` (server-side for `server_side` deploys) | live objects differ from git |
| | `kubectl get --show-managed-fields` and a server-side `apply --dry-run=server` | a hand-run kubectl command owns a field git doesn't set |
| helm deploy | `helm status` | the release is missing, not `deployed`, or on a different chart `version:` |
| | `helm upgrade --install --dry-run=server`, then the two manifest checks on its output | live objects differ from the rendered chart, or a hand-run kubectl command owns a field the chart doesn't set |
| | `helm get manifest` | the release has an object the chart no longer renders |

Plans skip the state lock, so a sweep never blocks a real apply. Their output
is captured, not printed; run `kest swoop plan <root>` to see a drifted root's
full plan. Each plan is recorded to swoop state, so `kest swoop list` shows
when roots were last checked.

### Live kubectl edits

Neither `kubectl diff` nor a server-side apply catches a field that someone
set with `kubectl patch` when your manifest never mentions that field: the diff
is empty and a re-apply leaves it in place. The managed-fields check catches it.
Any field manager named `kubectl` or `kubectl-*` (edit, patch, scale, set,
label, annotate, ...) counts as a live edit, but only for fields git doesn't
set. A server-side dry-run apply shows which fields those are. A field git does
set may still be co-owned by an old patch with the same value, which is
harmless, and `kubectl diff` reports it if the values differ. Two managers are exempt:
`kubectl-client-side-apply`, which is kest's own manager for `server_side:
false` deploys, and `kubectl-rollout`, which only stamps `restartedAt`.

To fold a live edit back into git, add the field to the manifest. To revert it,
remove the field from the object (`kubectl edit`), since a re-apply won't.

### Helm deploys

A helm deploy is rendered the way the next `kest deploy` would render it: a
server-side dry-run of `helm upgrade --install` with the deploy's chart,
version, values and `set:` entries. That needs helm 3.13 or newer and no
plugins. The rendered objects then go through the same `kubectl diff` and
managed-fields checks as a manifest deploy, so a changed value, an edit made
outside helm, and a `kubectl patch` of a field the chart never sets all show
up. Objects the chart places outside the release namespace are checked too.

Hooks aren't part of the rendered manifest and aren't compared. A chart that
generates a value on every render (a random password, a self-signed
certificate) reports drift on every sweep. A release that is mid-operation
(`pending-*`) reports only its status.

## Narrowing a sweep

- `--only terraform` / `--only deploys`
- `--dir <top-level dir>` limits roots, like `kest swoop --dir`
- `--target <target>` limits deploys
- `--exclude-root <path>` skips a root, or every root under a directory; `--exclude-deploy <name>` skips a deploy.
  Both repeat, and add to the [exclusions in `.kestconfig`](#exclusions).
- `--parallel N` sets how many checks run at once (default 4)
- `--init` runs `init` on each root first, one at a time, since terraform's
  plugin cache isn't safe for concurrent init. Without it, an uninitialized
  root is reported as an error.
- `--json` prints the results as a JSON array instead of the table

A `--dir` or `--target` that matches nothing is an error, so a typo can't
shrink the sweep to whatever is left and report it clean. An exclusion that
matches nothing is an error too.

### Exclusions

Checks a sweep should always skip belong in the project's `.kestconfig`, next
to the roots and deploys they describe, each with a reason:

```yaml
drift:
  exclude_roots:
    - path: prd              # a root, or every root under a directory
      reason: the CI role can't read prd state
      only: kestci
    - path: dev/dns
      reason: provider needs the VPN
  exclude_deploys:
    - name: grafana
      reason: chart generates a password on every render
```

`reason` is required. `only: kestci` or `only: kest` limits an entry to that
binary, for a check one posture can't run but the other can; without it the
entry applies to both. The global config can't set exclusions.

An excluded check stays in the table as `SKIPPED` with its reason, and in
`--json` with `"status": "skipped"`, so a sweep never reads as covering more
than it did. Skipped checks don't affect the exit code:

```
KIND    NAME     STATUS   DETAIL
root    dev/vpc  CLEAN    no changes
deploy  web      CLEAN
root    prd/rds  SKIPPED  the CI role can't read prd state

2 checked: 2 clean, 0 drifted, 0 errored; 1 skipped
```

A check excluded by flag is listed the same way, with the flag as its detail.
Only checks the other filters select are listed: with `--dir dev`, an excluded
`prd` root isn't. Every entry must still match a root or name a deploy, whichever
binary it applies to, so a renamed root can't leave a stale exclusion behind.

`kest drift` refreshes each AWS SSO session once up front. `kestci drift` uses
ambient credentials like the rest of kestci; a fresh checkout needs `--init`.

## In CI

```yaml
- name: Drift sweep
  run: kestci drift --init
```

The step fails on exit 1 or 2, which is the alert. For finer handling, branch
on the exit code as in
[`examples/ci/github-drift-detection.yml`](examples/ci/github-drift-detection.yml),
or parse `--json`. If the runner only has credentials for some roots, list the
rest under `drift.exclude_roots` with `only: kestci`, narrow with `--dir` or
`--only`, or keep a per-root matrix of `kestci plan -- -detailed-exitcode` as
that example does.
