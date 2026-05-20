set shell := ["bash", "-euo", "pipefail", "-c"]

# Project recipes for treeman.
#
# Common entry points:
#   just                    # show this list
#   just test               # cargo test --workspace
#   just lint               # clippy + fmt check
#   just fmt                # cargo fmt --all
#   just build              # debug build
#   just build-release      # release build
#   just run-daemon         # build + run treemand in foreground
#   just lock               # refresh Cargo.lock + flake.lock
#   just release-patch      # bump patch + lock + tag + push
#   just release-minor      # bump minor + lock + tag + push
#   just release-major      # bump major + lock + tag + push

default:
    @just --list --unsorted

# ───── dev ─────

test:
    cargo test --workspace --all-features

lint:
    cargo fmt --all -- --check
    cargo clippy --workspace --all-targets -- -D warnings

fmt:
    cargo fmt --all

build:
    cargo build --workspace

build-release:
    cargo build --workspace --release

run-daemon: build
    ./target/debug/treemand

# Install locally for ad-hoc use.
install:
    cargo install --path crates/treeman-cli --force
    cargo install --path crates/treeman-daemon --force

# Refresh both Cargo.lock and flake.lock (if nix is available).
lock:
    cargo update --workspace
    @if command -v nix >/dev/null 2>&1; then \
        nix flake update; \
    else \
        echo "nix not installed — skipping flake.lock update"; \
    fi

# ───── nix ─────

nix-build:
    nix build .#treeman .#treemand

nix-check:
    nix flake check --print-build-logs

nix-shell:
    nix develop

# ───── release ─────

# Bump version, refresh locks, commit, tag, push. The tag push triggers
# the GitHub Actions release workflow which builds binaries for Linux
# (x86_64 + aarch64) and macOS (x86_64 + aarch64) and attaches them to
# the GitHub release.

release-patch: (_release "patch")
release-minor: (_release "minor")
release-major: (_release "major")

_release bump:
    #!/usr/bin/env bash
    set -euo pipefail
    # 0. preflight: clean working tree on master/main
    branch=$(git rev-parse --abbrev-ref HEAD)
    if [[ "$branch" != "main" && "$branch" != "master" ]]; then
        echo "release must run on main/master, currently on '$branch'"; exit 1
    fi
    if [[ -n "$(git status --porcelain)" ]]; then
        echo "working tree dirty — commit or stash first"; exit 1
    fi
    if ! command -v cargo-set-version >/dev/null 2>&1 \
        && ! cargo set-version --help >/dev/null 2>&1; then
        echo "cargo-edit not installed (need 'cargo set-version'). Install: cargo install cargo-edit"
        exit 1
    fi
    # 1. bump the workspace version (touches every crate's Cargo.toml).
    cargo set-version --workspace --bump {{ bump }}
    NEW=$(awk -F'"' '/^version *=/ {print $2; exit}' Cargo.toml)
    echo "new version: v${NEW}"
    # 2. refresh Cargo.lock so the bumped versions land there too.
    cargo update --workspace
    # 3. refresh flake.lock if nix is available — keeps reproducible builds in sync.
    if command -v nix >/dev/null 2>&1; then
        nix flake update
    fi
    # 4. sanity: build + test before tagging.
    cargo build --workspace --release
    cargo test --workspace
    # 5. commit + tag + push.
    git add -A
    git commit -m "release v${NEW}"
    git tag -a "v${NEW}" -m "v${NEW}"
    git push
    git push --tags
    echo
    echo "tagged v${NEW} — release workflow will publish binaries shortly."
    echo "watch: gh run watch || open https://github.com/stubbe/treeman/actions"
