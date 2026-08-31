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

        # Go for the dev/CI shells. Pinned to the SAME major that nixpkgs
        # builds golangci-lint with: the linter loads packages through the
        # `go` on PATH, and a mismatch fails every package with
        # `compile: version go1.X does not match go tool version go1.Y`
        # rather than reporting real findings (nixpkgs currently ships
        # golangci-lint built with 1.27 while `pkgs.go` is 1.26). When a
        # nixpkgs bump moves the linter's toolchain, that error is the
        # signal to bump this line. Not used for the package build, which
        # goes through buildGoModule's own toolchain.
        shellGo = pkgs.go_1_27;

        treeman = pkgs.buildGoModule {
          pname = "treeman";
          version = "2.5.87";
          src = ./.;
          # buildGoModule fetches Go deps through the module proxy and
          # hashes the resulting vendor tree; `vendorHash` pins that
          # hash so the sandboxed build is reproducible. Bump after
          # any `go get` / `go mod tidy` that changes go.sum — `nix
          # build` will print the expected hash on mismatch.
          # go-sum: b5de61e1496c92ca922c87516921873a45f37cffe6bf63e7e89edc587e66dfea
          vendorHash = "sha256-DmEA2nEbeG6lfUj68S2RJ4K8XIWH4NzeihmWQ7syEK0=";
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
            "-X github.com/stubbedev/treeman/internal/version.Version=2.5.87"
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
          packages = [ shellGo ] ++ (with pkgs; [
            gopls
            golangci-lint
            just
            git
          ]);
          shellHook = ''
            echo "treeman dev shell — \`just go-build\` to compile, \`just go-test\` to test"
          '';
        };

        # Lean shell for CI's lint job: the SAME golangci-lint the dev
        # shell provides, minus the editor tooling. flake.lock is then the
        # single source of truth for the linter version — CI and every
        # developer run the identical binary, so a nixpkgs bump can't leave
        # CI on an older linter than `just check` (which is how a renamed
        # linter turned into 1481 local-only findings).
        devShells.ci = pkgs.mkShell {
          packages = [ shellGo pkgs.golangci-lint ];
        };

        formatter = pkgs.nixpkgs-fmt;
      });
}
