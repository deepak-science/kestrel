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
| | `kubectl get --show-managed-fields` | a field is owned by a hand-run kubectl command |
| helm deploy | `helm status` | the release is missing, not `deployed`, or on a different chart `version:` |
| | `helm diff upgrade --detailed-exitcode` | rendered objects differ (only with the [helm-diff](https://github.com/databus23/helm-diff) plugin) |

Plans skip the state lock, so a sweep never blocks a real apply. Their output
is captured, not printed; run `kest swoop plan <root>` to see a drifted root's
full plan. Each plan is recorded to swoop state, so `kest swoop list` shows
when roots were last checked.

### Live kubectl edits

Neither `kubectl diff` nor a server-side apply catches a field that someone
set with `kubectl patch` when your manifest never mentions that field: the diff
is empty and a re-apply leaves it in place. The managed-fields check catches it.
Any field manager named `kubectl` or `kubectl-*` (edit, patch, scale, set,
label, annotate, ...) counts as a live edit. Two are exempt:
`kubectl-client-side-apply`, which is kest's own manager for `server_side:
false` deploys, and `kubectl-rollout`, which only stamps `restartedAt`.

To fold a live edit back into git, add the field to the manifest. To revert it,
remove the field from the object (`kubectl edit`), since a re-apply won't.

### Helm without helm-diff

Without the plugin, a helm deploy is checked only for release status and chart
version, and a clean row says `values not compared`. Install the plugin to
diff values and rendered objects too.

## Narrowing a sweep

- `--only terraform` / `--only deploys`
- `--dir <top-level dir>` limits roots, like `kest swoop --dir`
- `--target <target>` limits deploys
- `--parallel N` sets how many checks run at once (default 4)
- `--init` runs `init` on each root first, one at a time, since terraform's
  plugin cache isn't safe for concurrent init. Without it, an uninitialized
  root is reported as an error.
- `--json` prints the results as a JSON array instead of the table

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
or parse `--json`. If the runner only has credentials for some roots, narrow
with `--dir` or `--only`, or keep a per-root matrix of `kestci plan --
-detailed-exitcode` as that example does.
