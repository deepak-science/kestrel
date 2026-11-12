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
      version = "0.4.2";
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
      # Runnable container image with kest + kestci and the tools they shell
      # out to. Build with `nix build .#docker`, then `docker load < result`.
      # Linux-only (dockerTools); the terraform version is pinned by the
      # image, so the version manager is disabled via env.
      dockerImageFor =
        pkgs:
        let
          # gitMinimal still ships with perl and python; strip further.
          gitReallyMinimal =
            (pkgs.git.override {
              perlSupport = false;
              pythonSupport = false;
              withManual = false;
              withpcre2 = false;
            }).overrideAttrs
              (_: {
                # installCheck is broken when perl is disabled
                doInstallCheck = false;
              });
        in
        pkgs.dockerTools.buildLayeredImage {
          name = "kest";
          tag = version;
          contents = with pkgs; [
            kest
            kestci
            terraform
            opentofu
            kubernetes-helm
            kubectl
            awscli2
            gitReallyMinimal
            gh
            bashInteractive
            coreutils
            dockerTools.caCertificates
            (dockerTools.fakeNss.override {
              extraPasswdLines = [ "kest:x:1000:1000:kest:/home/kest:/bin/bash" ];
              extraGroupLines = [ "kest:x:1000:" ];
            })
            dockerTools.usrBinEnv
            dockerTools.binSh
          ];
          extraCommands = ''
            mkdir -p tmp work home/kest
            chmod 1777 tmp
            # HOME is env-set rather than uid-derived so the image works
            # under any --user override (rootless docker maps the host
            # user to root; k8s may assign arbitrary uids). Keep the home
            # writable by all of them.
            chmod 0777 home/kest
            # /work is bind-mounted from the host, so its files usually
            # aren't owned by the container user — git refuses such repos
            # ("dubious ownership"), which would break kest's worktree
            # guards and tag resolution.
            printf '[safe]\n\tdirectory = /work\n' > home/kest/.gitconfig
          '';
          fakeRootCommands = ''
            chown -R 1000:1000 ./home/kest ./work
          '';
          config = {
            Cmd = [ "kest" ];
            User = "1000:1000";
            WorkingDir = "/work";
            Env = [
              "PATH=/usr/bin:/bin"
              "HOME=/home/kest"
              "USER=kest"
              "LOGNAME=kest"
              "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
              "KEST_TERRAFORM_VERSION_MANAGER=off"
            ];
            # Mirrors `hanko version docker labels`, baked in at nix build
            # time since the image isn't built by `docker build`.
            Labels = {
              "org.opencontainers.image.title" = "kest";
              "org.opencontainers.image.description" =
                "kest + kestci with helm, terraform, opentofu, kubectl, awscli, and git bundled";
              "org.opencontainers.image.version" = version;
              "org.opencontainers.image.revision" = self.rev or self.dirtyRev or "unknown";
              "org.opencontainers.image.source" = "https://github.com/deepak-science/kestrel";
            };
          };
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
          docker = dockerImageFor pkgs;
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
