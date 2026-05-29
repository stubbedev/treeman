{
  description = "treeman — per-worktree DB orchestrator (Go v1.0+)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };

        treeman = pkgs.buildGoModule {
          pname = "treeman";
          version = "2.4.8";
          src = ./.;
          # buildGoModule fetches Go deps through the module proxy and
          # hashes the resulting vendor tree; `vendorHash` pins that
          # hash so the sandboxed build is reproducible. Bump after
          # any `go get` / `go mod tidy` that changes go.sum — `nix
          # build` will print the expected hash on mismatch.
          # go-sum: 5980445213f7e36709c4eb8fa3ab76dad936eeda0f49a4d48de053ce69fa831b
          vendorHash = "sha256-DtnqngOquNn8eSW7kbB7vH/JBoZ4yiFq6LD9NSVuGsU=";
          # subPackages also scopes the default checkPhase — `go test`
          # only runs against these two paths (neither has test files),
          # so `nix build` / `nix profile install` finishes the check
          # phase in milliseconds instead of running the full unit
          # suite (let alone the docker-backed e2e suite, which is
          # `//go:build e2e`-gated and would fail in the sandbox).
          # DO NOT remove subPackages without also setting
          # `doCheck = false` — otherwise install will start running
          # internal/* tests against an empty sandboxed environment.
          subPackages = [ "cmd/treeman" "cmd/treemand" ];
          ldflags = [
            "-s"
            "-w"
            "-X github.com/stubbedev/treeman/internal/version.Version=2.4.8"
          ];
          doCheck = true;
        };
      in
      {
        packages = {
          default = treeman;
          treeman = treeman;
          treemand = treeman; # same derivation; subPackages covers both binaries
        };

        apps.default = flake-utils.lib.mkApp {
          drv = treeman;
          name = "treeman";
        };

        checks.build = treeman;

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            golangci-lint
            just
            git
          ];
          shellHook = ''
            echo "treeman dev shell — \`just go-build\` to compile, \`just go-test\` to test"
          '';
        };

        formatter = pkgs.nixpkgs-fmt;
      });
}
