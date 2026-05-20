{
  description = "treeman — per-worktree DB orchestrator with file watcher";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    rust-overlay = {
      url = "github:oxalica/rust-overlay";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    crane = {
      url = "github:ipetkov/crane";
    };
  };

  outputs = { self, nixpkgs, flake-utils, rust-overlay, crane }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
          overlays = [ (import rust-overlay) ];
        };

        rustToolchain = pkgs.rust-bin.fromRustupToolchainFile ./rust-toolchain.toml;
        craneLib = (crane.mkLib pkgs).overrideToolchain rustToolchain;

        # Single source of truth — bumped by `cargo set-version` in release recipes.
        cargoVersion = (builtins.fromTOML (builtins.readFile ./Cargo.toml)).workspace.package.version;

        # `cleanCargoSource` keeps only files crane recognises (.rs,
        # Cargo.toml, etc.). The treeman-store crate embeds its SQLite
        # schema via `sqlx::migrate!("./migrations")` at compile time;
        # without including `.sql` files in the source tree, the nix-
        # built binary ships with an empty migrations vector and the
        # daemon creates the database file without any schema.
        # Override the filter so `migrations/*.sql` is kept.
        srcRoot = ./.;
        src = pkgs.lib.cleanSourceWith {
          src = srcRoot;
          filter = path: type:
            let baseName = baseNameOf path; in
            (pkgs.lib.hasSuffix ".sql" baseName)
            || (craneLib.filterCargoSources path type);
          name = "treeman-source";
        };

        commonArgs = {
          inherit src;
          pname = "treeman";
          version = cargoVersion;
          strictDeps = true;
          nativeBuildInputs = with pkgs; [
            pkg-config
            perl  # openssl-sys build script
          ];
          buildInputs = with pkgs; [
            openssl
            sqlite
          ] ++ pkgs.lib.optionals pkgs.stdenv.isDarwin [
            pkgs.darwin.apple_sdk.frameworks.Security
            pkgs.darwin.apple_sdk.frameworks.SystemConfiguration
            pkgs.libiconv
          ];
        };

        cargoArtifacts = craneLib.buildDepsOnly commonArgs;

        treemanWorkspace = craneLib.buildPackage (commonArgs // {
          inherit cargoArtifacts;
          doCheck = false;  # `nix flake check` runs tests separately
          # Build every member of the workspace.
          cargoExtraArgs = "--workspace --locked";
        });

        # Individually-named packages (just symlinks into the workspace
        # output) for convenience: `nix build .#treeman`, `.#treemand`.
        bin = name: pkgs.runCommand name { } ''
          mkdir -p $out/bin
          cp ${treemanWorkspace}/bin/${name} $out/bin/${name}
        '';

      in
      {
        # Go binaries — built via buildGoModule. These are the
        # v1.0+ artefacts; the Rust derivations above stay for the
        # transition window. Switch the `default` package to the Go
        # build once feature parity lands.
        treemanGo = pkgs.buildGoModule {
          pname = "treeman-go";
          version = cargoVersion;
          src = srcRoot;
          # `vendorHash = null` because we will commit go.sum and use
          # the Go module proxy; bump to a fixed hash if/when we add
          # third-party deps.
          vendorHash = null;
          subPackages = [ "cmd/treeman" "cmd/treemand" ];
          ldflags = [
            "-s"
            "-w"
            "-X github.com/stubbedev/treeman/internal/version.Version=${cargoVersion}"
          ];
          doCheck = true;
        };

        packages = {
          default = treemanWorkspace;
          treeman = bin "treeman";
          treemand = bin "treemand";
          workspace = treemanWorkspace;

          # `nix build .#treeman-go` for the Go rewrite preview.
          treeman-go = treemanGo;
        };

        apps = {
          default = flake-utils.lib.mkApp {
            drv = self.packages.${system}.treeman;
            name = "treeman";
          };
          treeman = flake-utils.lib.mkApp {
            drv = self.packages.${system}.treeman;
            name = "treeman";
          };
          treemand = flake-utils.lib.mkApp {
            drv = self.packages.${system}.treemand;
            name = "treemand";
          };
        };

        # `nix flake check` validates:
        #   - the workspace package builds under Nix (catches buildInput
        #     drift, missing system deps, sandbox-path issues that bare
        #     cargo wouldn't see).
        #   - the source passes rustfmt.
        # Clippy + tests intentionally NOT duplicated here — the bare
        # cargo `clippy` + `test` jobs in `.github/workflows/ci.yml` use
        # `Swatinem/rust-cache` and run the same checks ~10× faster.
        # Including them again would mean three workspace rebuilds per
        # CI run.
        checks = {
          inherit treemanWorkspace;
          treeman-fmt = craneLib.cargoFmt {
            inherit src;
            pname = "treeman";
            version = cargoVersion;
          };
        };

        devShells.default = pkgs.mkShell {
          inputsFrom = [ treemanWorkspace ];
          packages = with pkgs; [
            rustToolchain
            just
            cargo-edit
            cargo-watch
            cargo-sweep
            sqlx-cli
            git
            # Build-perf toolchain. `.cargo/config.toml` wires clang+mold
            # as the linker for x86_64-linux; sccache caches rustc output
            # across `cargo clean` cycles.
            mold
            clang
            sccache
            # Go toolchain for the v1.0 rewrite.
            go
            gopls
            golangci-lint
          ];
          shellHook = ''
            echo "treeman dev shell — try \`just --list\`"
          '';
        };

        formatter = pkgs.nixpkgs-fmt;
      });
}
