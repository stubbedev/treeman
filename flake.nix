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

        # Sqlx 0.8 in offline mode needs nothing; in online mode (queries
        # validated at compile against a real DB) it'd need DATABASE_URL.
        # treeman doesn't use the sqlx::query! macro, so plain
        # cleanCargoSource is enough.
        src = craneLib.cleanCargoSource ./.;

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
        packages = {
          default = treemanWorkspace;
          treeman = bin "treeman";
          treemand = bin "treemand";
          workspace = treemanWorkspace;
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

        checks = {
          inherit treemanWorkspace;
          treeman-test = craneLib.cargoTest (commonArgs // {
            inherit cargoArtifacts;
            cargoExtraArgs = "--workspace";
          });
          treeman-clippy = craneLib.cargoClippy (commonArgs // {
            inherit cargoArtifacts;
            cargoClippyExtraArgs = "--workspace --all-targets -- -D warnings";
          });
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
            sqlx-cli
            git
          ];
          shellHook = ''
            echo "treeman dev shell — try \`just --list\`"
          '';
        };

        formatter = pkgs.nixpkgs-fmt;
      });
}
