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
          version = "1.0.0-dev";
          src = ./.;
          # `vendorHash = null` because go.sum + Go module proxy are
          # the authoritative dep source. Update to a fixed hash if
          # nix sandbox builds need to vendor offline.
          vendorHash = null;
          subPackages = [ "cmd/treeman" "cmd/treemand" ];
          ldflags = [
            "-s"
            "-w"
            "-X github.com/stubbedev/treeman/internal/version.Version=1.0.0-dev"
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
