# justfile for treeman
# Run `just` to see all available commands.

set shell := ["bash", "-euo", "pipefail", "-c"]

# Configuration
CARGO_BIN := "treeman"
DAEMON_BIN := "treemand"

# Default recipe — list all available commands.
default:
    @just --list --unsorted

# =============================================================================
# Setup & Dependencies
# =============================================================================

# Verify the local environment has every tool the recipes assume.
check-tools:
    #!/usr/bin/env bash
    set -euo pipefail
    fail=0
    need() {
        if ! command -v "$1" >/dev/null 2>&1; then
            echo "missing: $1 ($2)" >&2; fail=1
        fi
    }
    need cargo "https://rustup.rs"
    need git "system package manager"
    need gh "https://cli.github.com"
    if ! cargo set-version --help >/dev/null 2>&1; then
        echo "missing: cargo-edit (provides cargo set-version)" >&2
        echo "  install: cargo install cargo-edit" >&2
        fail=1
    fi
    command -v nix >/dev/null 2>&1 || echo "(optional) nix not installed — flake recipes will be skipped"
    [ "$fail" = "0" ] || exit 1
    echo "All required tools present."

# =============================================================================
# Build & Test — Go (treeman v1.0+ — see plan in
# ~/.claude/plans/for-both-setup-and-resilient-meerkat.md)
# =============================================================================

# Go binaries: cmd/treeman + cmd/treemand. The version string is
# baked in via -ldflags so `treeman --version` reports the right tag.
GO_LDFLAGS := "-X github.com/stubbedev/treeman/internal/version.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)"

# Build both Go binaries into ./bin/.
go-build:
    mkdir -p bin
    go build -ldflags="{{GO_LDFLAGS}}" -o bin/treeman  ./cmd/treeman
    go build -ldflags="{{GO_LDFLAGS}}" -o bin/treemand ./cmd/treemand
    @echo "Built ./bin/treeman + ./bin/treemand"

# Install the Go binaries into $GOBIN (~/.go/bin or $HOME/go/bin).
go-install:
    go install -ldflags="{{GO_LDFLAGS}}" ./cmd/treeman ./cmd/treemand
    @echo "Installed treeman + treemand to $(go env GOBIN || echo $(go env GOPATH)/bin)"

go-fmt:
    gofmt -w ./cmd ./internal

go-lint:
    gofmt -l ./cmd ./internal | tee /dev/stderr | (! grep -q '.')
    go vet ./...

go-test:
    go test ./...

go-check: go-lint go-test

go-clean:
    rm -rf bin/

# =============================================================================
# Build & Test — Rust (legacy; will be removed after Go v1.0 cutover)
# =============================================================================

# Cargo workspace build (debug).
build:
    cargo build --workspace

# Cargo workspace build (release).
build-release:
    cargo build --workspace --release

# Install treeman + treemand into ~/.cargo/bin.
install: build-release
    cargo install --path crates/treeman-cli --force --locked
    cargo install --path crates/treeman-daemon --force --locked
    @echo "Installed treeman + treemand to $(cargo env CARGO_HOME 2>/dev/null || echo \"\$HOME/.cargo\")/bin"

# Format every workspace member.
fmt:
    cargo fmt --all

# rustfmt --check + clippy with warnings as errors.
lint:
    cargo fmt --all -- --check
    cargo clippy --workspace --all-targets -- -D warnings

# Run the workspace test suite.
test:
    cargo test --workspace --all-features

# CI-parity: fmt + lint + test in one shot.
check: lint test

# Foreground daemon for local dev/debugging.
run-daemon: build
    ./target/debug/{{DAEMON_BIN}}

# Remove build artifacts.
clean:
    cargo clean
    rm -f coverage.out coverage.html

# Refresh Cargo.lock and (if nix is available) flake.lock.
lock:
    cargo update --workspace
    @if command -v nix >/dev/null 2>&1; then \
        nix flake update; \
    else \
        echo "(nix not installed — skipping flake.lock update)"; \
    fi

# =============================================================================
# Nix
# =============================================================================

nix-build:
    nix build .#treeman .#treemand

nix-check:
    nix flake check --print-build-logs

nix-shell:
    nix develop

# =============================================================================
# Release
# =============================================================================

# Show what the next versions would be for each bump (dry-run).
release-preview:
    #!/usr/bin/env bash
    set -euo pipefail
    CURRENT_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
    CURRENT_VERSION=${CURRENT_TAG#v}
    MAJOR=$(echo "$CURRENT_VERSION" | cut -d. -f1)
    MINOR=$(echo "$CURRENT_VERSION" | cut -d. -f2)
    PATCH=$(echo "$CURRENT_VERSION" | cut -d. -f3)
    echo "Current tag:    $CURRENT_TAG"
    echo "Cargo version:  $(sed -n 's/^version *= *"\(.*\)".*/\1/p' Cargo.toml | head -1)"
    echo
    echo "  release-major: v$((MAJOR + 1)).0.0"
    echo "  release-minor: v${MAJOR}.$((MINOR + 1)).0"
    echo "  release-patch: v${MAJOR}.${MINOR}.$((PATCH + 1))"

# Private preflight: clean tree on default branch, autocommit any
# drift, refresh locks. Run by every release-{major,minor,patch}.
_release-checks:
    #!/usr/bin/env bash
    set -euo pipefail
    just check-tools

    BRANCH=$(git rev-parse --abbrev-ref HEAD)
    DEFAULT_BRANCH=$(git rev-parse --abbrev-ref origin/HEAD 2>/dev/null \
        | sed 's|^origin/||' || true)
    if [ -z "${DEFAULT_BRANCH:-}" ]; then
        DEFAULT_BRANCH=$(git remote show origin 2>/dev/null \
            | awk '/HEAD branch/ {print $NF}' || echo main)
    fi
    if [ "$BRANCH" != "$DEFAULT_BRANCH" ]; then
        echo "Error: not on default branch '$DEFAULT_BRANCH' (currently '$BRANCH')." >&2
        exit 1
    fi

    just check
    if [ -n "$(git status --porcelain)" ]; then
        echo "Formatting/lint produced changes — staging + committing."
        git add -A
        git commit -m "chore: format code for release"
    fi

    if command -v nix >/dev/null 2>&1; then
        echo "Refreshing flake.lock..."
        nix flake update
        if [ -n "$(git status --porcelain flake.lock)" ]; then
            git add flake.lock
            git commit -m "chore: update flake.lock for release"
        fi
        echo "Verifying nix build..."
        # Crane derives all hashes from Cargo.lock automatically; no
        # vendorHash to patch. If the build fails we just bail.
        nix build --no-link .#workspace
    else
        echo "(nix not installed — skipping flake checks)"
    fi

# Bump workspace version in Cargo.toml, refresh Cargo.lock,
# commit, tag, push, push --tags. The tag push triggers
# .github/workflows/release.yml which builds + uploads binaries.
_release bump:
    #!/usr/bin/env bash
    set -euo pipefail
    just _release-checks
    cargo set-version --workspace --bump {{ bump }}
    NEW=$(sed -n 's/^version *= *"\(.*\)".*/\1/p' Cargo.toml | head -1)
    cargo update --workspace
    cargo build --workspace --release
    git add -A
    git commit -m "release v${NEW}"
    git tag -a "v${NEW}" -m "v${NEW}"
    git push origin HEAD
    git push origin "v${NEW}"
    echo
    echo "Tagged v${NEW}."
    echo "Watch the release build: gh run watch || open https://github.com/stubbe/treeman/actions"

# Release a new patch version (x.y.Z -> x.y.Z+1).
release-patch: (_release "patch")

# Release a new minor version (x.Y.z -> x.(Y+1).0).
release-minor: (_release "minor")

# Release a new major version (X.y.z -> (X+1).0.0).
release-major: (_release "major")
