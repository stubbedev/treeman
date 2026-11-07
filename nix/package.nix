{
  perSystem = { go, lib, pkgs, ... }:
    let
      version = "2.5.123";
      treeman = pkgs.buildGoModule.override { inherit go; } {
        pname = "treeman";
        inherit version;
        src = lib.fileset.toSource {
          root = ../.;
          fileset = lib.fileset.difference (lib.fileset.gitTracked ../.) (
            lib.fileset.unions [
              ../flake.nix
              ../flake.lock
              ../nix
            ]
          );
        };
        # buildGoModule fetches Go deps through the module proxy and
        # hashes the resulting vendor tree; `vendorHash` pins that
        # hash so the sandboxed build is reproducible. Bump after
        # any `go get` / `go mod tidy` that changes go.sum — `nix
        # build` will print the expected hash on mismatch.
        # go-sum: 8b813902d595c57343cc81049d7d6d4a90faee28193553c7bc9bd44d7f45f5c7
        vendorHash = "sha256-gplhwtBtxb4Vg+x9BcwdGYwubk1uENkaXGdfNiTi57g=";
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
          "-X github.com/stubbedev/treeman/internal/version.Version=${version}"
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

      apps.default = {
        type = "app";
        program = lib.getExe' treeman "treeman";
      };

      checks.build = treeman;
    };
}
