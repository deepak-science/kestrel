# kest

Kestrel (`kest`) is a CLI that wraps Helm and Terraform so you don't have to remember which AWS profile goes with which cluster, or what directory your terraform roots live in.
You set up a `.kestconfig` in your project and a global config on your machine, and kest figures out the rest.

It's particularly useful if you're deploying the same app to multiple EKS clusters across different AWS accounts, or managing a centralized IaC repo with dozens of terraform roots.

## What it does

- **App deploys** — roll out helm charts or raw manifests to any resolved cluster, with explicit values file layering and deploy scripts
- **Terraform orchestration** ("swoop") — discovers terraform roots in your repo, lets you pick them interactively or target them with globs, and tracks init/plan/apply state
- **Profile management** — set a target once, and all subsequent commands use it (no more `-e dev` on every invocation)
- **Safety guards** — CI-only enforcement, clean worktree checks, prod-only-from-main restrictions
- **`kest exec`** — run any command with the right AWS_PROFILE and kube context already set

## Install

Quick install (linux-amd64, linux-arm64, darwin-arm64):

```sh
curl -fsSL https://github.com/deepak-science/kestrel/releases/latest/download/install.sh | bash
```

See [`scripts/install.sh`](scripts/install.sh) or run it with `--help` for options.

### Docker

Every release also publishes a container image to ghcr with `kest`, `kestci`, and the tools they shell out to (terraform, opentofu, helm, kubectl, aws, git) bundled:

```sh
docker run --rm ghcr.io/deepak-science/kestrel:latest kest --version
```

Tags follow the release version (`0.2.0`, `0.2`, `0`, `latest`), multi-arch for linux amd64/arm64. Mount your project and AWS config to do real work:

```sh
docker run --rm -it \
  -v "$PWD:/work" \
  -v "$HOME/.aws:/home/kest/.aws" \
  ghcr.io/deepak-science/kestrel kest doctor
```

See [docs/docker.md](docs/docker.md) for full running instructions — which config paths to mount where, AWS SSO from a container, and using `kestci` as a CI job image.

The image pins its own terraform and opentofu (repos with `terraform.command: tofu` just work), so kest's version-manager integration is disabled inside it (`KEST_TERRAFORM_VERSION_MANAGER=off`).

Build it locally on linux with `just docker` (`nix build .#docker` + `docker load`).

### Building from source

Kest is built with Nix:

```sh
nix build    # produces ./result/bin/kest
```

Or if you just want to hack on it:

```sh
nix develop  # drops you into a shell with Go, terraform, helm, etc.
go build -o kest .
```

## Setup

Kest uses two config files: a global one on your machine and a project one (`.kestconfig`) committed to your repo. Every field has a default; the blocks below show every field with its default value. A repo or machine with no config file is treated as if it had exactly the defaults below.

### Global config (`~/.config/kest/config.yaml`)

```yaml
# --- AWS ---
aws:
  # Map AWS account IDs to profile names from ~/.aws/config. Example:
  #   accounts:
  #     "111122223333":
  #       aws_profile: dev-sso
  #     "444455556666":
  #       aws_profile: prd-sso
  accounts: {}
  # Automatically run `aws sso login` when a session is expired.
  # Skipped in CI. Without a TTY, kest exits with the login command to run instead.
  auto_sso_login: false

# --- Kubernetes ---
kubernetes:
  # Map short cluster names to full kube context strings (typically EKS ARNs).
  # Example:
  #   contexts:
  #     eks-dev: arn:aws:eks:us-east-1:111122223333:cluster/eks-dev
  #     kind-local: kind-local
  contexts: {}

# --- Terraform execution ---
terraform:
  # Terraform-compatible CLI to invoke. Set to "tofu" to use OpenTofu.
  # Overridden at runtime by $KEST_TERRAFORM_COMMAND.
  command: "terraform"
  # Version-manager CLI kest uses for version-pin handling.
  # "tfenv", "tofuenv", or "off". Empty auto-detects: "tofuenv" when the
  # resolved command is "tofu", else "tfenv". Overridden by
  # $KEST_TERRAFORM_VERSION_MANAGER. See "Terraform vs OpenTofu" below.
  version_manager: ""
  # Automatically install the pinned terraform version (from the
  # version-pin file) via the configured version_manager on mismatch,
  # without prompting. No-op when version_manager is "off". Skipped in CI.
  auto_install_pinned: false
  # Write a version-pin file (.opentofu-version when version_manager is
  # tofuenv, else .terraform-version) into roots that lack one before
  # running init/plan/apply.
  write_version: false
  # Version to write when write_version is enabled. Empty means detect the
  # currently active terraform version at write time.
  default_version: ""

# --- Swoop (interactive terraform root browser) ---
swoop:
  # Shell command emitted by `swoop cd`: "cd" or "pushd".
  cd_mode: cd
  # Editor for `swoop edit`. Empty means use $EDITOR.
  editor: ""
  # Root ordering in the TUI: "git" (dirty-first + recency), "recent",
  # or "alpha".
  sort_order: git
```

You can generate the accounts/contexts automatically with `kest config autoconfigure`, which scans your `~/.aws/config` and `~/.kube/config` and walks you through a TUI to pick what you want.

### Project config (`.kestconfig`)

Goes in your project root (committed to git). Kest walks up from your current directory to find it, so it works from anywhere in your project tree.

```yaml
# --- Helm (shared deploy settings) ---
helm:
  # Scripts to run before each deploy (paths relative to project root).
  # Deploys inherit these; override per deploy (set deploy_scripts: [] to skip).
  # Example:
  #   deploy_scripts:
  #     - misc/chart/deploy-scripts/migrate.sh
  deploy_scripts: []

# --- Deploys (apps: helm charts or raw manifests, cluster-agnostic) ---
# The app-deploy map driving `kest deploy` / `kestci deploy`. Each entry sets
# exactly one source — chart: (helm) or manifests: (kubectl); kest picks the
# executor. Targets can be EKS or any named context. See docs/deploy.md.
# Example:
#   deploys:
#     homepage:                       # local in-repo chart
#       chart: charts/app
#       values: [deploys/homepage.yaml]
#       namespace: homepage
#       target: homelab
#     authentik:                      # third-party chart from a repo
#       chart: authentik/authentik
#       repo: https://charts.goauthentik.io
#       version: "2024.10.1"
#       values: [deploys/authentik.yaml]
#       target: homelab
#     gitea:                          # raw manifests (kubectl apply -f dir/)
#       manifests: k8s-manifests/gitea
#       target: homelab
deploys: {}

# --- Terraform ---
terraform:
  # Path to IaC directory (swoop discovery base for centralised IaC repos).
  # Empty means the project root. Example: misc/iac
  iac_dir: ""
  # command can also be set here to pin the CLI per-project (e.g. "tofu").
  # Project overrides global. $KEST_TERRAFORM_COMMAND overrides both.
  command: ""
  # version_manager can also be set here to pin the manager per-project.
  # Project overrides global. $KEST_TERRAFORM_VERSION_MANAGER overrides both.
  version_manager: ""
  # default_version can also be set here to pin a project-wide terraform
  # version for write_version.
  default_version: ""

# --- Targets ---
# Named deployment targets binding a cluster + AWS account + region. Deploys
# reference these by key; swoop also resolves through them. Example:
#   targets:
#     dev:
#       cluster: eks-dev             # must resolve via kubernetes.contexts
#       aws_account: "111122223333"  # must resolve via aws.accounts
#       region: us-east-1
#     local:
#       cluster: kind-local          # cluster-only targets are valid (no AWS)
#     homelab:
#       cluster: my-cluster          # a named context (kind/k3s/…), used as-is
#       kubeconfig: iac-live/cluster/kubeconfig  # optional explicit kubeconfig
#                                    # (project-relative) for `kest deploy`
targets: {}

# --- Directories (swoop only) ---
# For centralised-IaC repos: map a top-level directory name to an AWS
# account ID, so swoop can resolve credentials by path even when a root
# has no provider/account in its .tf files. Example:
#   directories:
#     prd: "444455556666"
#     dev: "111122223333"
directories: {}
```

### Terraform vs OpenTofu

By default kest invokes `terraform`. To use OpenTofu instead, set
`terraform.command: tofu` (in either the global config or `.kestconfig`)
or run with `KEST_TERRAFORM_COMMAND=tofu` for a one-off swap. Precedence
is `$KEST_TERRAFORM_COMMAND` → project `.kestconfig` → global config →
`"terraform"`.

The `version_manager` knob picks which companion CLI kest probes for
pinning workflows. Pick whichever your repo uses; kest itself stays binary-
agnostic:

- `tfenv` (default when command is `terraform`) — reads `.terraform-version`
- `tofuenv` (auto-default when command is `tofu`) — reads `.opentofu-version`
- `off` — disable kest's version-manager integration entirely: no PATH
  probe, no install offers, no mismatch warnings about a missing manager

When `write_version: true`, kest writes the file the resolved manager
reads — `.opentofu-version` for tofuenv, `.terraform-version` otherwise.
Discovery reads either file (`.opentofu-version` wins when both exist), so
a repo migrating between the two still recognises its pin during the
transition.

## Usage

### Profiles

Instead of passing `-e dev` every time:

```sh
kest profile use          # interactive picker
kest profile set dev      # non-interactive
kest profile current      # show what's active

# export to your shell
eval "$(kest profile export)"
```

### Deploy (apps: helm charts + raw manifests)

`kest deploy` applies an app defined under `deploys:`, picking its executor
automatically — a helm chart (`helm upgrade --install`) or a directory of raw
manifests (`kubectl apply -f`):

```sh
kest deploy gitea                    # kubectl apply -f k8s-manifests/gitea/
kest deploy homepage                 # helm upgrade --install (local or third-party chart)
kest deploy homepage --diff          # read-only preview (helm --dry-run / kubectl diff)
kest deploy --all --target homelab   # all apps on a target
kest deploy gitea -- --server-side   # pass extra args to kubectl/helm
```

It shares target resolution and the guard stack with everything else, and is
cluster-agnostic: a target's `cluster` is a named kube context (resolved via
`kubernetes.contexts` or used literally — an EKS ARN or any other context),
with an optional explicit `kubeconfig`. Helm deploys layer their values files in
order and run any configured deploy scripts. In CI use `kestci deploy` (ambient
credentials, guards always on). Full guide: [docs/deploy.md](docs/deploy.md);
worked example combining terraform + helm + manifests:
[examples/deploy/](examples/deploy/).

### Terraform

For simple project-embedded terraform:

```sh
kest -e dev terraform plan
kest -e dev terraform apply
```

### Swoop (terraform root discovery)

For repos with lots of terraform roots, swoop discovers them automatically by walking for `.tf` files with backend blocks:

```sh
kest swoop                      # interactive TUI picker
kest swoop list                 # list all roots
kest swoop list --dir prd       # filter by top-level directory
kest swoop plan "live/dev/*"    # glob targeting
kest swoop plan infra           # substring match
kest swoop plan --changed       # only roots with git changes
kest swoop edit dev/vpc         # open $EDITOR in root directory
eval "$(kest swoop cd dev/vpc)" # cd into root directory
```

Swoop tracks the last time you ran init/plan/apply on each root, so you can see at a glance what's stale. The TUI also supports `e` to edit and `c` to cd directly from the root picker.

### Running arbitrary commands

```sh
kest -e dev exec -- kubectl get pods
kest -e prod exec -- aws sts get-caller-identity
```

This sets up the right AWS_PROFILE and kube context before running your command.

### Config inspection

```sh
kest config paths       # where config files are (and if they loaded)
kest config show        # merged config as YAML
kest config targets     # list all targets with resolved context/profile
kest config accounts    # list account ID mappings
```

## How resolution works

When you run `kest deploy myapp`, here's what happens:

1. Kest finds your `.kestconfig` and looks up the `myapp` deploy → gets target `dev`
2. Looks up the `dev` target → gets cluster name `eks-dev`, account `111122223333`
3. Looks up `eks-dev` in your global config's contexts → gets the full EKS ARN
4. Looks up that account ID in your global config's accounts → gets `aws_profile: dev-sso`
5. Runs helm/kubectl with the deploy's values files, the right kube context, and AWS_PROFILE set

For swoop, resolution works through directory→account ID mappings instead, but the account→profile lookup is the same.

## Development

```sh
just build    # nix build
just test     # run all tests
just check    # nix flake check
just fmt      # format nix files
just chores   # go mod tidy + regenerate gomod2nix.toml
```

Run a specific test:

```sh
go test ./internal/config/ -run TestName
```

After changing `go.mod`, always run `just chores` to keep `gomod2nix.toml` in sync.

## Project structure

```
cmd/           CLI layer (Cobra commands)
internal/
  config/      Two-layer config loading and target resolution
  profile/     Active profile persistence
  deploy/      App deploy spine (helm chart / kubectl manifest executors)
  terraform/   Terraform command proxying
  guard/       Deploy safety checks
  runner/      External command execution
  swoop/       Terraform root discovery, resolution, execution, state tracking
  logging/     Structured JSON logging
  awsconfig/   AWS config file parsing
  kubeconfig/  Kube config file parsing
```
