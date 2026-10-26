{
  perSystem = { go, pkgs, ... }:
    let
      mkdocs = pkgs.python3.withPackages (ps: [ ps.mkdocs-material ]);
    in
    {
      devShells.default = pkgs.mkShell {
        packages = [ go mkdocs ] ++ (with pkgs; [
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
        packages = [ go pkgs.golangci-lint ];
      };

      # The docs site build: regenerates the generated reference pages
      # (go) and renders them with MkDocs Material. Shared with the
      # default shell so `just docs-serve` matches what Pages publishes.
      devShells.docs = pkgs.mkShell {
        packages = [ go mkdocs pkgs.just ];
      };

      formatter = pkgs.nixpkgs-fmt;
    };
}
