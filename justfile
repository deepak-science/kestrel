# executes default, set to listing commands
default:
    just --list

# build the kest and kestci binaries via nix
build:
    nix build .#default .#kestci

# run go tests via nix develop
test:
    nix flake check
    # go test ./...

# run nix flake check
check:
    nix flake check

# format nix files
fmt:
    nix fmt

# update flake inputs
update:
    nix flake update

# regenerate gomod2nix.toml and tidy go modules
chores:
    nix develop --command go mod tidy
    nix develop --command gomod2nix

# release: stamp flake.nix, commit, tag, push — all via `hanko seal`.
# Reads .hanko.yaml for stamp-targets + seal config.
# Preview with `just release-plan` before running for real.
release:
    nix develop --command hanko seal

# release-plan: print what `just release` would do without mutating anything.
release-plan:
    nix develop --command hanko seal --dry-run
