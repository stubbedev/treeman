# justfile for treeman (Go v1.0+)
# Run `just` to see all available commands.

set shell := ["bash", "-euo", "pipefail", "-c"]

# Default — list recipes.
default:
    @just --list --unsorted

# ─────────────────────────── Build & Test ───────────────────────────

# Version baked into the binary at link time.
GO_LDFLAGS := "-X github.com/stubbedev/treeman/internal/version.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)"

# Build both binaries into ./bin/.
build:
    mkdir -p bin
    go build -ldflags="{{GO_LDFLAGS}}" -o bin/treeman  ./cmd/treeman
    go build -ldflags="{{GO_LDFLAGS}}" -o bin/treemand ./cmd/treemand
    @echo "Built ./bin/treeman + ./bin/treemand"

# Install into $GOBIN (or $GOPATH/bin).
install:
    go install -ldflags="{{GO_LDFLAGS}}" ./cmd/treeman ./cmd/treemand
    @echo "Installed treeman + treemand to $(go env GOBIN || echo $(go env GOPATH)/bin)"

fmt:
    gofmt -w ./cmd ./internal

# rustfmt-equivalent — fails CI if anything is unformatted.
# Prints the diff on failure so the file + change is obvious.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(gofmt -l ./cmd ./internal)
    if [ -n "$out" ]; then
        echo "gofmt would rewrite:"
        printf '%s\n' "$out"
        echo
        gofmt -d ./cmd ./internal
        exit 1
    fi
    go vet ./...

test:
    go test ./...

check: lint test

clean:
    rm -rf bin/

# ─────────────────────────── Nix ───────────────────────────

nix-build:
    nix build .#treeman

nix-check:
    nix flake check --print-build-logs

# Rewrite flake.nix `vendorHash` to match the current go.sum and
# bump `version` + the linker -X flag. With no argument, version is
# read from `git describe --tags --always --dirty`. With an argument
# (used by the release recipe), version is set to that literal.
#
# The hash sync works by: (1) overwriting vendorHash with a sentinel,
# (2) running `nix build` which prints the actual hash on mismatch,
# (3) parsing that hash and writing it back.
sync-flake version="":
    #!/usr/bin/env bash
    set -euo pipefail
    VERSION="{{version}}"
    if [ -z "$VERSION" ]; then
        VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
        VERSION=${VERSION#v}
    fi
    SENTINEL="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
    # Stamp the sentinel so `nix build` reports the expected hash.
    sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$SENTINEL"'";|' flake.nix
    set +e
    OUT=$(nix build .#treeman --no-link 2>&1)
    set -e
    NEW_HASH=$(printf '%s\n' "$OUT" | awk '/got:[[:space:]]*sha256-/ {print $2; exit}')
    if [ -z "$NEW_HASH" ]; then
        # Sentinel already matched the real hash — pull it back from
        # what we just wrote (nothing to change for vendorHash).
        NEW_HASH="$SENTINEL"
        # But if nix build genuinely failed for some other reason,
        # surface that error.
        if ! printf '%s\n' "$OUT" | grep -q "hash mismatch\|all checks passed\|/nix/store/"; then
            echo "$OUT" >&2
            echo "sync-flake: nix build failed with no hash to capture" >&2
            exit 1
        fi
    fi
    sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$NEW_HASH"'";|' flake.nix
    # Version + ldflags string.
    sed -i -E 's|^(\s*version = )"[^"]*";|\1"'"$VERSION"'";|' flake.nix
    sed -i -E 's|(-X github.com/stubbedev/treeman/internal/version.Version=)[^"]*|\1'"$VERSION"'|' flake.nix
    echo "sync-flake: vendorHash=$NEW_HASH version=$VERSION"
    # Final sanity: a real build must pass with the new hash.
    nix build .#treeman --no-link

# ─────────────────────────── Release ───────────────────────────

release-preview:
    #!/usr/bin/env bash
    set -euo pipefail
    CURRENT_TAG=$(git tag -l 'v*.*.*' --sort=-v:refname | head -1)
    CURRENT_TAG=${CURRENT_TAG:-v0.0.0}
    CURRENT_VERSION=${CURRENT_TAG#v}
    MAJOR=$(echo "$CURRENT_VERSION" | cut -d. -f1)
    MINOR=$(echo "$CURRENT_VERSION" | cut -d. -f2)
    PATCH=$(echo "$CURRENT_VERSION" | cut -d. -f3)
    echo "Current tag: $CURRENT_TAG"
    echo "  release-major: v$((MAJOR + 1)).0.0"
    echo "  release-minor: v${MAJOR}.$((MINOR + 1)).0"
    echo "  release-patch: v${MAJOR}.${MINOR}.$((PATCH + 1))"

_release-checks:
    #!/usr/bin/env bash
    set -euo pipefail
    BRANCH=$(git rev-parse --abbrev-ref HEAD)
    DEFAULT_BRANCH=$(git rev-parse --abbrev-ref origin/HEAD 2>/dev/null | sed 's|^origin/||' || true)
    if [ -z "${DEFAULT_BRANCH:-}" ]; then
        DEFAULT_BRANCH=$(git remote show origin 2>/dev/null | awk '/HEAD branch/ {print $NF}' || echo master)
    fi
    if [ "$BRANCH" != "$DEFAULT_BRANCH" ] && [ "$BRANCH" != "go-rewrite" ]; then
        echo "Error: not on default branch '$DEFAULT_BRANCH' (currently '$BRANCH')." >&2
        exit 1
    fi
    just check
    if [ -n "$(git status --porcelain)" ]; then
        echo "Formatting/lint produced changes — staging + committing."
        git add -A
        git commit -m "chore: format code for release"
    fi

_release bump:
    #!/usr/bin/env bash
    set -euo pipefail
    just _release-checks
    CURRENT_TAG=$(git tag -l 'v*.*.*' --sort=-v:refname | head -1)
    CURRENT_TAG=${CURRENT_TAG:-v0.0.0}
    CURRENT_VERSION=${CURRENT_TAG#v}
    MAJOR=$(echo "$CURRENT_VERSION" | cut -d. -f1)
    MINOR=$(echo "$CURRENT_VERSION" | cut -d. -f2)
    PATCH=$(echo "$CURRENT_VERSION" | cut -d. -f3)
    case "{{bump}}" in
        major) NEW="$((MAJOR + 1)).0.0" ;;
        minor) NEW="${MAJOR}.$((MINOR + 1)).0" ;;
        patch) NEW="${MAJOR}.${MINOR}.$((PATCH + 1))" ;;
        *) echo "unknown bump kind: {{bump}}"; exit 1 ;;
    esac
    # Always sync flake.nix vendorHash + version BEFORE tagging.
    # Even when go.sum hasn't changed, the version + ldflags strings
    # must reflect v${NEW} or `nix profile install` will report a
    # stale version. sync-flake re-validates the build at the end.
    just sync-flake "${NEW}"
    if [ -n "$(git status --porcelain flake.nix)" ]; then
        git add flake.nix
        git commit -m "chore: bump flake.nix to v${NEW}"
    fi
    git tag -a "v${NEW}" -m "v${NEW}"
    git push origin HEAD
    git push origin "v${NEW}"
    echo
    echo "Tagged v${NEW}."
    echo "Watch the release build: gh run watch || open https://github.com/stubbedev/treeman/actions"

release-patch: (_release "patch")
release-minor: (_release "minor")
release-major: (_release "major")
