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
          version = "2.2.20";
          src = ./.;
          # buildGoModule fetches Go deps through the module proxy and
          # hashes the resulting vendor tree; `vendorHash` pins that
          # hash so the sandboxed build is reproducible. Bump after
          # any `go get` / `go mod tidy` that changes go.sum — `nix
          # build` will print the expected hash on mismatch.
          # go-sum: 0e9ddc04cfed78bac95a9cdb51e70ef8024b34537d787a5584c6d4c95fa8d60d
          vendorHash = "sha256-vmFr+0PZohtoYugKWaiVxycvZKYMspWKH87a0cbFT5I=";
          subPackages = [ "cmd/treeman" "cmd/treemand" ];
          ldflags = [
            "-s"
            "-w"
            "-X github.com/stubbedev/treeman/internal/version.Version=2.2.20"
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
