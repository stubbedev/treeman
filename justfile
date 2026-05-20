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
lint:
    gofmt -l ./cmd ./internal | tee /dev/stderr | (! grep -q '.')
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

# ─────────────────────────── Release ───────────────────────────

release-preview:
    #!/usr/bin/env bash
    set -euo pipefail
    CURRENT_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
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
    CURRENT_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
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
    git tag -a "v${NEW}" -m "v${NEW}"
    git push origin HEAD
    git push origin "v${NEW}"
    echo
    echo "Tagged v${NEW}."
    echo "Watch the release build: gh run watch || open https://github.com/stubbedev/treeman/actions"

release-patch: (_release "patch")
release-minor: (_release "minor")
release-major: (_release "major")
