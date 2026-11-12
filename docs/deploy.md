# `kest deploy` — app deploys (helm charts + raw manifests)

`kest deploy` applies an **app** to a resolved cluster, picking its executor
automatically: a helm chart (`helm upgrade --install`) or a directory of raw
manifests (`kubectl apply -f`). It's the cluster-agnostic, multi-app path —
kind, k3s, bare named contexts, EKS — with the same target resolution, guard
stack, and ambient-vs-profile policy that `kest swoop` uses for terraform.

It's built for "a bunch of apps, each its own chart or manifest set, on whatever
cluster" — each `deploys:` entry names one app and one source. For the design
rationale see [deploy-routines.md](deploy-routines.md).

## Config

Apps go under `deploys:` in `.kestconfig`. Each entry sets **exactly one**
source — `chart:` (helm) or `manifests:` (kubectl):

```yaml
deploys:
  # 1. local in-repo chart (a shared chart, or per-app — your call)
  homepage:
    chart: charts/app                 # local path, oci:// ref, or repo chart name
    values: [deploys/homepage.yaml]   # values files, layered in order (project-root relative)
    namespace: homepage               # optional; adds --create-namespace
    target: homelab

  # 2. third-party chart from a repo
  authentik:
    chart: authentik/authentik
    repo: https://charts.goauthentik.io
    version: "2024.10.1"
    values: [deploys/authentik.yaml]
    set:                              # optional inline --set overrides
      authentik.error_reporting.enabled: "false"
    target: homelab

  # 3. raw manifests (applied in NN- filename order, like kubectl apply -f dir/)
  gitea:
    manifests: k8s-manifests/gitea
    target: homelab

targets:
  homelab:
    cluster: my-cluster               # a named kube context (kind, k3s, …),
                                      # resolved via kubernetes.contexts or used
                                      # literally — not necessarily an EKS ARN
    kubeconfig: iac-live/cluster/kubeconfig   # optional explicit kubeconfig
```

Fields, per deploy:

| Field | Executor | Meaning |
| --- | --- | --- |
| `chart` | helm | local path, `oci://…` ref, or chart name (with `repo`) |
| `repo` | helm | chart repository URL (`--repo`) |
| `version` | helm | chart version (`--version`) |
| `release_name` | helm | helm release name (default: the deploy's key) |
| `values` | helm | values files, project-root relative, layered in order |
| `set` | helm | inline `--set key=value` overrides |
| `manifests` | kubectl | directory or file applied with `kubectl apply -f` |
| `server_side` | kubectl | apply and diff with `--server-side --field-manager=kest` (see below) |
| `namespace` | both | `--namespace` (+ `--create-namespace` for helm) |
| `target` | both | **required** — key in `targets:` |
| `deploy_scripts` | both | override `helm.deploy_scripts` (`nil` = inherit, `[]` = skip) |
| `snapshot_class` | both | VolumeSnapshotClass for `kest snapshot` / `--snapshot` (default: per-driver default class) |
| `snapshot_skip` | both | PVCs to leave out of snapshots: globs on the PVC name, or `namespace/name` |

### Target resolution (cluster-agnostic)

A target's `cluster` is looked up in your global `kubernetes.contexts`; if it
isn't mapped, the literal value is used as the context name. So a bare named
context like `my-cluster` "just works" once it's in your kubeconfig (e.g. merged
in by hand or by a `just kubeconfig` helper). The optional `kubeconfig:` points
at an explicit file — handy in CI, or to read a terraform-output kubeconfig
without merging it into `~/.kube/config`. AWS profile resolution is unchanged:
for an EKS target it resolves the account's profile, and it stays empty for
non-AWS clusters.

## Usage

```sh
kest deploy gitea                    # kubectl apply -f k8s-manifests/gitea/
kest deploy homepage                 # helm upgrade --install homepage charts/app …
kest deploy homepage --diff          # read-only preview: helm --dry-run / kubectl diff
kest deploy gitea --wait             # apply, then wait for every workload rollout
kest deploy gitea --force-conflicts  # server_side deploys: take back fields edited live
kest deploy gitea --snapshot         # snapshot the app's volumes first; abort if that fails
kest snapshot gitea                  # just the snapshots
kest deploy --all                    # every app
kest deploy --all --target homelab   # every app on a target
kest deploy gitea -- --validate=strict  # pass extra args straight to kubectl/helm
kest deploy gitea --force            # bypass guards (CI-only, clean-worktree, branch)
```

`--diff` is the plan analog: `helm upgrade --dry-run` for charts, `kubectl diff`
for manifests (a present diff exits non-zero — kest treats that as "there's a
diff", not a failure). It's read-only and skips the guard stack.

`--wait` (manifest deploys) runs `kubectl rollout status` on every Deployment,
StatefulSet and DaemonSet in the manifest dir after the apply, then prints a
summary with each workload's images. Every workload is waited on even if one
fails; any failure exits non-zero. `--wait-timeout` sets the per-workload
timeout (default `5m`). Helm deploys already wait via `--atomic`, so `--wait`
is a no-op there. Works the same on `kestci deploy`.

### Server-side apply (`server_side: true`)

With `server_side: true` on a manifest deploy, kest applies as field manager `kest`. It also prints its own change summary. kubectl reports every server-side object as `serverside-applied`, so kest compares each object's `resourceVersion` before and after the apply:

```
configmap/cfg unchanged
deployment.apps/web changed

0 created, 1 changed, 1 unchanged
```

What changes compared with client-side apply:

- **Live edits to fields kest manages fail loudly.** If someone ran `kubectl set image` or `kubectl patch` on a field the manifest sets, apply and `--diff` both fail with a conflict naming the field and the other manager. `--force-conflicts` takes the field back and reverts it to the manifest.
- **Fields removed from the manifest are removed from the object**, provided kest was their manager.
- **Fields other managers added and the manifest never mentions are left alone.** For example, a `nodeSelector` patched in by hand survives a re-apply, and `--diff` won't show it. Neither client-side nor server-side apply reconciles these fields; delete them by hand or recreate the object.

Switching an existing deploy is a no-op apply: kubectl migrates the client-side `last-applied` ownership to `kest` on the first server-side run. Don't switch back afterwards. Client-side apply on a server-side-managed object warns about the missing annotation and re-adds it.

### Volume snapshots (`kest snapshot`, `--snapshot`)

`kest snapshot <app>` takes a CSI `VolumeSnapshot` (`snapshot.storage.k8s.io/v1`) of every bound PVC in the app's namespace. The namespace is the deploy's `namespace:`, or for manifest deploys without one, the namespaces of the manifests' existing objects. Helm deploys need `namespace:` set. kest then waits until each snapshot is `readyToUse` (`--timeout`, default `10m`):

```
snapshot: gitea/data-gitea-0 → data-gitea-0-kest-20261111-142530 (class csi-snapclass)
snapshot: 1 ready
```

`kest deploy --snapshot` (and `kestci deploy --snapshot`) does the same before an apply, ahead of any deploy scripts, and aborts the deploy if a snapshot fails or times out. `--diff` never snapshots.

- **Requirements:** the cluster needs the CSI snapshot CRDs and snapshot controller ([external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter)) plus a CSI driver that supports snapshots. kest stays storage-agnostic; it doesn't use storage-specific backup CRDs.
- **Class selection:** each PVC uses `snapshot_class:` if set, which must match its storage driver. Otherwise it uses the VolumeSnapshotClass marked default for its driver, or the driver's only class.
- **All or nothing:** if any bound PVC can't be snapshotted (no class for its driver, e.g. a local-path volume), kest creates nothing and lists the PVCs it couldn't snapshot. A pre-upgrade backup that silently skips a volume is worse than none. To leave a volume out on purpose (an NFS share, a cache), list it under `snapshot_skip:`; kest prints each skipped PVC.

  ```yaml
  deploys:
    media:
      manifests: k8s/media
      namespace: media
      target: homelab
      snapshot_skip: [library, "cache-*"]
  ```
- **Retention:** snapshots are labeled `app.kubernetes.io/managed-by=kest` and `kest/deploy=<app>`. kest never deletes them; prune with `kubectl delete volumesnapshot -l kest/deploy=<app>`.
- **Restore** is a normal CSI restore: create a PVC with `dataSource` pointing at the snapshot.

### Guards

Applies go through the standard guard stack: CI-only, clean worktree, and
prod-only-from-main. `--force` bypasses all three for local applies. `--diff`
is exempt (read-only).

## In CI (`kestci deploy`)

`kestci deploy <app>` is the non-interactive sibling: identical resolution, but
**ambient** credentials (it never sets `AWS_PROFILE`; kubeconfig comes from the
environment) and guards are always enforced — there is no `--force`.

```sh
kestci deploy authentik              # apply, ambient creds, clean-worktree + branch guards
kestci deploy --all --target homelab
kestci deploy gitea --diff           # read-only
```

Supply the kubeconfig the way that fits your runner: a `KUBECONFIG` secret, an
explicit `kubeconfig:` on the target (e.g. a terraform-output file written by an
earlier job step), or — for EKS — a step that writes a kubeconfig for the
cluster before `kestci deploy`. See [ci.md](ci.md) for the blast-radius tiering
that decides
*which* clusters should be driven from CI at all (Tier-2 platform/SSO stacks
usually shouldn't).

## What `deploy` is not

- **Not an image-tag pipeline.** `kest deploy` applies what's in your chart and
  values as-is — no image-tag resolution and no automatic values layering beyond
  the `values:` list you give it. Bake your own tags into values or pass `--set`.
- **Not GitOps.** No `--prune`, no drift reconcile — `--diff` is the only read
  path. Argo/Flux own continuous reconciliation if you want it.
- **Not the platform.** Cluster-tier helm (traefik, cert-manager, longhorn, …)
  belongs in terraform `helm_release` where its state lives. `deploys:` is for
  apps running *on* the platform.
