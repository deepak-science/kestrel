{
  description = "kestrel (kest) CLI dev environment";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    gomod2nix = {
      url = "github:nix-community/gomod2nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    hanko = {
      url = "github:deepak-science/hanko";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };
  outputs =
    {
      self,
      nixpkgs,
      gomod2nix,
      hanko,
      ...
    }@inputs:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
        "x86_64-darwin"
      ];
      eachSystem = f: nixpkgs.lib.genAttrs supportedSystems (system: f (pkgsFor system));
      pkgsFor =
        system:
        let
          pkgs = import nixpkgs {
            inherit system;
            config.allowUnfree = true;
          };
        in
        pkgs.extend (
          nixpkgs.lib.composeManyExtensions [
            gomod2nix.overlays.default
            kestOverlay
          ]
        );
      treefmtEval = eachSystem (pkgs: inputs.treefmt-nix.lib.evalModule pkgs ./treefmt.nix);
      # Stamped by hanko; do not hand-edit (use `just release`).
      # kest and kestci ship as one product — both inherit this version.
      version = "1.0.0";
      commonLdflags = [
        "-s"
        "-w"
        "-X"
        "main.version=${version}"
        "-X"
        "main.commit=${self.rev or self.dirtyRev or "unknown"}"
        "-X"
        "main.date=${self.lastModifiedDate or "unknown"}"
      ];
      # Cross-built on Linux so releases don't need a macOS runner.
      # Go's linker ad-hoc signs darwin/arm64 binaries, which is all Apple silicon needs to run them.
      darwinArm64 =
        name: pkg:
        pkg.overrideAttrs {
          GOOS = "darwin";
          GOARCH = "arm64";
          doCheck = false;
          dontStrip = true;
          # go install puts cross binaries in bin/darwin_arm64; completions can't be generated without running them.
          postInstall = ''
            mv $out/bin/darwin_arm64/* $out/bin/${name}
            rmdir $out/bin/darwin_arm64
          '';
        };
      kestOverlay = final: _prev: {
        kest = final.buildGoApplication {
          pname = "kest";
          inherit version;
          src = ./.;
          modules = ./gomod2nix.toml;
          subPackages = [ "." ];
          nativeBuildInputs = [ final.installShellFiles ];
          CGO_ENABLED = 0;
          ldflags = commonLdflags;
          postInstall = ''
            mv $out/bin/kestrel $out/bin/kest
            installShellCompletion --cmd kest \
              --bash <($out/bin/kest completion bash) \
              --zsh <($out/bin/kest completion zsh) \
              --fish <($out/bin/kest completion fish)
          '';
        };
        kestci = final.buildGoApplication {
          pname = "kestci";
          inherit version;
          src = ./.;
          modules = ./gomod2nix.toml;
          subPackages = [ "cmd/kestci" ];
          nativeBuildInputs = [ final.installShellFiles ];
          CGO_ENABLED = 0;
          ldflags = commonLdflags;
          postInstall = ''
            installShellCompletion --cmd kestci \
              --bash <($out/bin/kestci completion bash) \
              --zsh <($out/bin/kestci completion zsh) \
              --fish <($out/bin/kestci completion fish)
          '';
        };
      };
    in
    {

      overlays.default = nixpkgs.lib.composeManyExtensions [
        gomod2nix.overlays.default
        kestOverlay
      ];

      packages = eachSystem (
        pkgs:
        {
          default = pkgs.kest;
          kestci = pkgs.kestci;
        }
        // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          kest-darwin-arm64 = darwinArm64 "kest" pkgs.kest;
          kestci-darwin-arm64 = darwinArm64 "kestci" pkgs.kestci;
        }
      );

      formatter = eachSystem (pkgs: treefmtEval.${pkgs.stdenv.hostPlatform.system}.config.build.wrapper);
      checks = eachSystem (pkgs: {
        formatting = treefmtEval.${pkgs.stdenv.hostPlatform.system}.config.build.check self;
        # Lint as a check: override the kest derivation to swap go test for
        # golangci-lint in checkPhase. Reuses the vendored module setup from
        # goConfigHook so no network is needed inside the sandbox.
        golangci-lint = pkgs.kest.overrideAttrs (old: {
          pname = "kest-golangci-lint";
          nativeCheckInputs = (old.nativeCheckInputs or [ ]) ++ [ pkgs.golangci-lint ];
          doCheck = true;
          checkPhase = ''
            runHook preCheck
            export GOLANGCI_LINT_CACHE=$TMPDIR/golangci-lint-cache
            golangci-lint run --timeout 5m ./...
            runHook postCheck
          '';
        });
        # Compat smoke: exercises the terraform/tofu binary-override surface
        # by running real terraform and opentofu binaries through kest.
        terraform-compat =
          pkgs.runCommand "kest-terraform-compat"
            {
              nativeBuildInputs = [
                pkgs.kest
                pkgs.kestci
                pkgs.terraform
                pkgs.opentofu
                pkgs.git
                pkgs.bash
              ];
            }
            ''
              export HOME=$TMPDIR
              export KESTCI=${pkgs.kestci}/bin/kestci
              bash ${./test/compat/compat.sh} ${pkgs.kest}/bin/kest
              touch $out
            '';
        # Deploy smoke: exercises `kest deploy` / `kestci deploy` executor
        # selection + resolution end-to-end. kubectl and helm are stubbed by
        # the script itself, so no cluster (or those binaries) are needed.
        deploy-smoke =
          pkgs.runCommand "kest-deploy-smoke"
            {
              nativeBuildInputs = [
                pkgs.kest
                pkgs.kestci
                pkgs.bash
              ];
            }
            ''
              export HOME=$TMPDIR
              export KESTCI=${pkgs.kestci}/bin/kestci
              bash ${./test/deploy/deploy.sh} ${pkgs.kest}/bin/kest
              touch $out
            '';
      });

      devShells = eachSystem (pkgs: {
        default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gotools
            golangci-lint
            go-tools
            gopls
            terraform
            opentofu
            kubernetes-helm
            kubectl
            awscli2
            gomod2nix.packages.${pkgs.stdenv.hostPlatform.system}.default
            hanko.packages.${pkgs.stdenv.hostPlatform.system}.default
          ];
          shellHook = ''
            echo "kestrel devshell"
          '';
        };
      });
    };
}
