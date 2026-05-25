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

# Auto-fix formatting drift, then vet. Same dev contract as the
# sync-schema / sync-docs / sync-flake recipes: anything that *can*
# be regenerated *is* regenerated, and the release flow commits the
# diff. CI uses a separate read-only gofmt check (in ci.yml) as the
# strict gate so a broken `just lint` never silently re-fixes the
# CI workspace.
lint: fmt
    go vet ./...

# Strict read-only gofmt check — same logic CI runs, exposed for
# local pre-push verification.
lint-check:
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

# Run the e2e suite against real docker-backed engines. Pulls
# ~3GB of images on first run (mysql, postgres, mongo, redis, es)
# and binds local ports 13306-13356 + 15432 + 27117 + 16379 + 19200
# — make sure those are free. Each subtest brings up + tears down
# its own docker-compose stack.
test-e2e:
    go test -tags=e2e ./e2e/... -timeout 30m

check: lint test sync-schema sync-docs sync-flake

# Regenerate `docs/cli.md` from the live cli.Command tree. Catches
# command/flag drift in PRs by comparing the rewrite against what's
# in git. Same pattern as sync-schema — cheap enough to run on
# every `just check`.
sync-docs:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p docs
    go run ./cmd/treeman-gen-docs docs/cli.md
    if [ -n "$(git status --porcelain docs/cli.md)" ]; then
        echo "sync-docs: regenerated docs/cli.md"
    else
        echo "sync-docs: docs/cli.md already in sync"
    fi

# Regenerate `schemas/treeman.schema.json` from the current
# config.Config Go types. Cheap (pure reflection) so we run it on
# every `just check` to keep the canonical schema URL in sync with
# the binary on disk. CI asserts no drift on PRs and auto-commits
# on master pushes.
sync-schema:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p schemas
    go run ./cmd/treeman schema dump --out schemas/treeman.schema.json
    if [ -n "$(git status --porcelain schemas/treeman.schema.json)" ]; then
        echo "sync-schema: regenerated schemas/treeman.schema.json"
    else
        echo "sync-schema: schemas/treeman.schema.json already in sync"
    fi

clean:
    rm -rf bin/

# ─────────────────────────── Nix ───────────────────────────

nix-build:
    nix build .#treeman

nix-check:
    nix flake check --print-build-logs

# Keep flake.nix's `vendorHash` aligned with the current go.sum.
#
# A sha256 of go.sum is embedded as a `# go-sum:` line in flake.nix.
# When the cached digest matches go.sum on disk, sync-flake returns
# immediately without running `nix build`. That makes it cheap
# enough to run on every `just check`, so a dev `go get` flow can
# never push a master commit that breaks nix CI on master.
#
# By default this does NOT touch the version string — release-only
# concern. Pass an explicit `version` argument to also rewrite
# `version = "…"` + the `-X .../Version=…` ldflag (used by the
# release recipes). Pass `--force` to bypass the cache and re-run
# the nix build even if go.sum looks unchanged.
sync-flake version="":
    #!/usr/bin/env bash
    set -euo pipefail
    ARG="{{version}}"
    FORCE=0
    VERSION=""
    case "$ARG" in
        "")          ;;
        "--force")   FORCE=1 ;;
        *)           VERSION="${ARG#v}" ;;
    esac

    GO_SUM_HASH=$(sha256sum go.sum | awk '{print $1}')
    CACHED_HASH=$(awk -F': ' '/^[[:space:]]*#[[:space:]]*go-sum:/ {print $2; exit}' flake.nix | tr -d ' ')
    CURRENT_VERSION=$(awk -F'"' '/^[[:space:]]*version = "/ {print $2; exit}' flake.nix)

    NEED_HASH=0
    NEED_VERSION=0
    if [ "$FORCE" = "1" ] || [ "$GO_SUM_HASH" != "$CACHED_HASH" ]; then NEED_HASH=1; fi
    if [ -n "$VERSION" ] && [ "$VERSION" != "$CURRENT_VERSION" ]; then NEED_VERSION=1; fi

    if [ "$NEED_HASH" = "0" ] && [ "$NEED_VERSION" = "0" ]; then
        echo "sync-flake: up-to-date (go.sum=$GO_SUM_HASH version=$CURRENT_VERSION)"
        exit 0
    fi

    echo "sync-flake: refreshing (need_hash=$NEED_HASH need_version=$NEED_VERSION)"

    if [ "$NEED_HASH" = "1" ]; then
        SENTINEL="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
        sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$SENTINEL"'";|' flake.nix
        set +e
        OUT=$(nix build .#treeman --no-link 2>&1)
        BUILD_STATUS=$?
        set -e
        NEW_HASH=$(printf '%s\n' "$OUT" | awk '/got:[[:space:]]*sha256-/ {print $2; exit}')
        if [ -z "$NEW_HASH" ]; then
            if [ "$BUILD_STATUS" = "0" ]; then
                echo "sync-flake: unexpected nix build success with sentinel hash" >&2
                echo "$OUT" >&2
                exit 1
            fi
            echo "$OUT" >&2
            echo "sync-flake: nix build failed without printing 'got: sha256-…'" >&2
            exit 1
        fi
        sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$NEW_HASH"'";|' flake.nix
        if grep -q '^[[:space:]]*# go-sum:' flake.nix; then
            sed -i -E 's|^(\s*# go-sum:).*|\1 '"$GO_SUM_HASH"'|' flake.nix
        else
            sed -i -E 's|^(\s*vendorHash = )|          # go-sum: '"$GO_SUM_HASH"'\n\1|' flake.nix
        fi
        echo "sync-flake: vendorHash=$NEW_HASH go-sum=$GO_SUM_HASH"
    fi

    # Hard guard: refuse to leave the sentinel in flake.nix. If we
    # somehow get here with it still present, the working tree is
    # broken and CI will fail on hash mismatch — fail loudly here so
    # the dev catches it before push.
    if grep -q '^\s*vendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="' flake.nix; then
        echo "sync-flake: refusing to leave sentinel vendorHash in flake.nix" >&2
        exit 1
    fi

    if [ "$NEED_VERSION" = "1" ]; then
        sed -i -E 's|^(\s*version = )"[^"]*";|\1"'"$VERSION"'";|' flake.nix
        sed -i -E 's|(-X github.com/stubbedev/treeman/internal/version.Version=)[^"]*|\1'"$VERSION"'|' flake.nix
        echo "sync-flake: version=$VERSION"
    fi

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
