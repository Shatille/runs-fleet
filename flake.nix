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
          docker = runs-fleet-docker;
          admin-ui = admin-ui;
          default = runs-fleet-server;
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_1_25
            gopls
            gotools
            go-tools
            golangci-lint
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
          ];

          shellHook = ''
            echo "runs-fleet development environment"
            echo "Go version: $(go version)"
            echo ""
            echo "Available commands:"
            echo "  make build        - Build all binaries"
            echo "  make test         - Run tests"
            echo "  make lint         - Run linter"
            echo "  make docker-build - Build Docker image"
            echo "  make run-server   - Run server locally"
            echo ""
            echo "Nix packages:"
            echo "  nix build .#server      - Build server"
            echo "  nix build .#agent-amd64 - Build AMD64 agent"
            echo "  nix build .#agent-arm64 - Build ARM64 agent"
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
