{
  description = "Self-hosted ephemeral GitHub Actions runners on AWS";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          config.allowUnfreePredicate = pkg: builtins.elem (pkgs.lib.getName pkg) [
            "packer"
          ];
        };

        version = "0.1.0";

        # Pinned to the version CI installs (.github/workflows/ci.yml), because a
        # linter that disagrees with CI is worse than none: 2.12 reports ~1100
        # goconst findings on this tree that 2.9 does not. Keep both in step when
        # bumping. The builder is forced to Go 1.26 (nixpkgs pins an older line)
        # since golangci-lint refuses to load a config whose go.mod targets a newer
        # Go language version than the one it was built with.
        golangciLintVersion = "2.9.0";
        golangci-lint-pinned =
          (pkgs.golangci-lint.override { buildGo125Module = pkgs.buildGo126Module; }).overrideAttrs
            (old: rec {
              version = golangciLintVersion;
              src = pkgs.fetchFromGitHub {
                owner = "golangci";
                repo = "golangci-lint";
                tag = "v${version}";
                hash = "sha256-8LEtm1v0slKwdLBtS41OilKJLXytSxcI9fUlZbj5Gfw=";
              };
              vendorHash = "sha256-w8JfF6n1ylrU652HEv/cYdsOdDZz9J2uRQDqxObyhkY=";
              ldflags = (old.ldflags or [ ]) ++ [
                "-X main.version=${version}"
              ];
            });

        # Build admin UI (Next.js static export)
        admin-ui = pkgs.buildNpmPackage {
          pname = "runs-fleet-admin-ui";
          inherit version;
          src = ./pkg/admin/ui;

          npmDepsHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="; # Update after first build

          buildPhase = ''
            npm run build
          '';

          installPhase = ''
            mkdir -p $out
            cp -r out/* $out/
          '';

          meta = {
            description = "Admin UI for runs-fleet pool management";
          };
        };

        runs-fleet-server = pkgs.buildGoModule {
          pname = "runs-fleet-server";
          inherit version;
          src = ./.;

          vendorHash = null;

          subPackages = [ "cmd/server" ];

          ldflags = [
            "-s"
            "-w"
            "-extldflags=-static"
          ];

          CGO_ENABLED = "0";

          meta = {
            description = "Fleet orchestration server for ephemeral GitHub Actions runners";
            homepage = "https://github.com/Shavakan/runs-fleet";
            license = pkgs.lib.licenses.mit;
          };
        };

        runs-fleet-agent = arch: pkgs.buildGoModule {
          pname = "runs-fleet-agent-${arch}";
          inherit version;
          src = ./.;

          vendorHash = null;

          subPackages = [ "cmd/agent" ];

          ldflags = [
            "-s"
            "-w"
            "-extldflags=-static"
          ];

          CGO_ENABLED = "0";
          GOOS = "linux";
          GOARCH = arch;

          meta = {
            description = "Agent binary for GitHub Actions runners (${arch})";
            homepage = "https://github.com/Shavakan/runs-fleet";
            license = pkgs.lib.licenses.mit;
          };
        };

        runs-fleet-buildx-shim = arch: pkgs.buildGoModule {
          pname = "runs-fleet-buildx-shim-${arch}";
          inherit version;
          src = ./.;

          vendorHash = null;

          subPackages = [ "cmd/buildx-shim" ];

          ldflags = [
            "-s"
            "-w"
            "-extldflags=-static"
          ];

          CGO_ENABLED = "0";
          GOOS = "linux";
          GOARCH = arch;

          meta = {
            description = "Transparent buildx layer-cache shim for GitHub Actions runners (${arch})";
            homepage = "https://github.com/Shavakan/runs-fleet";
            license = pkgs.lib.licenses.mit;
          };
        };

        runs-fleet-docker = pkgs.dockerTools.buildImage {
          name = "runs-fleet";
          tag = "latest";

          config = {
            Cmd = [ "${runs-fleet-server}/bin/server" ];
            ExposedPorts = {
              "8080/tcp" = {};
            };
            Env = [
              "AWS_REGION=ap-northeast-1"
              "RUNS_FLEET_LOG_LEVEL=info"
            ];
          };
        };

      in
      {
        packages = {
          server = runs-fleet-server;
          agent-amd64 = runs-fleet-agent "amd64";
          agent-arm64 = runs-fleet-agent "arm64";
          buildx-shim-amd64 = runs-fleet-buildx-shim "amd64";
          buildx-shim-arm64 = runs-fleet-buildx-shim "arm64";
          docker = runs-fleet-docker;
          admin-ui = admin-ui;
          golangci-lint = golangci-lint-pinned;
          default = runs-fleet-server;
        };

        devShells.default = pkgs.mkShell {
          buildInputs = [ golangci-lint-pinned ] ++ (with pkgs; [
            go_1_26
            gopls
            gotools
            go-tools
            delve

            docker
            docker-compose

            awscli2
            ssm-session-manager-plugin
            packer

            gnumake

            nodejs_20

            jq
            yq
            actionlint
          ]);

          shellHook = ''
            echo "runs-fleet development environment"
            echo "Go version: $(go version)"
            echo ""
            echo "Available commands:"
            echo "  make build        - Build all binaries"
            echo "  make test         - Run tests"
            echo "  make lint         - Run linter (golangci-lint ${golangciLintVersion}, matches CI)"
            echo "  make docker-build - Build Docker image"
            echo "  make run-server   - Run server locally"
            echo ""
            echo "Nix packages:"
            echo "  nix build .#server      - Build server"
            echo "  nix build .#agent-amd64 - Build AMD64 agent"
            echo "  nix build .#agent-arm64 - Build ARM64 agent"
            echo "  nix build .#buildx-shim-amd64 - Build AMD64 buildx shim"
            echo "  nix build .#buildx-shim-arm64 - Build ARM64 buildx shim"
            echo "  nix build .#docker      - Build Docker image"
          '';

          AWS_REGION = "ap-northeast-1";
        };

        apps = {
          server = {
            type = "app";
            program = "${runs-fleet-server}/bin/server";
          };
        };
      }
    );
}
