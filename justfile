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
    golangci-lint fmt ./...

# Point git at .githooks/ so the pre-commit format gate fires. One-shot
# per clone; idempotent. CI still runs the same check as the
# authoritative gate — the hook just catches drift earlier.
install-hooks:
    #!/usr/bin/env bash
    set -euo pipefail
    git config core.hooksPath .githooks
    echo "git config core.hooksPath = .githooks"
    echo "pre-commit golangci-lint fmt gate is now active (bypass with --no-verify)."

# Auto-fix formatting drift (golangci-lint fmt = gofumpt + goimports +
# golines), then vet + the full golangci-lint gate. Same dev contract as
# the sync-schema / sync-docs / sync-flake recipes: anything that *can* be
# regenerated *is* regenerated. CI uses the read-only `lint-check` variant
# as the strict gate so a broken `just lint` never silently re-fixes the
# CI workspace.
lint: fmt
    go vet ./...
    golangci-lint run ./...

# Strict read-only check — same gates CI's lint job runs, exposed for
# local pre-push verification. Fails if the config references a
# renamed/removed linter, if formatting would change, or if any linter
# fires.
#
# CI runs these through the FLAKE-PINNED linter
# (`nix develop .#ci --command golangci-lint …`), so flake.lock decides the
# version for everyone. Reproduce CI exactly with:
#   nix develop .#ci --command golangci-lint run ./...
lint-check:
    #!/usr/bin/env bash
    set -euo pipefail
    golangci-lint config verify
    if ! golangci-lint fmt --diff; then
        echo "code is not formatted; run 'just fmt'" >&2
        exit 1
    fi
    go vet ./...
    golangci-lint run ./...

test:
    go test ./...

# Run the e2e suite inside a throwaway docker-in-docker sandbox: its own
# dockerd, /tmp, HOME and treeman state, with the checkout mounted
# read-only, so nothing a test does reaches this machine's daemon,
# registry, ports or docker. Images and Go caches persist in the
# treeman-e2e-* docker volumes. The harness refuses to run outside it.
#
# First arg picks one batch (see `just test-e2e-list`), one suite under
# e2e/, or `all`. Second arg sets how many suites run at once.
test-e2e batch="all" parallel="4":
    #!/usr/bin/env bash
    set -euo pipefail
    go_version=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2 }' go.mod)
    docker build -q -t treeman-e2e-sandbox --build-arg GO_VERSION="$go_version" e2e/sandbox >/dev/null
    tty=()
    if [ -t 0 ] && [ -t 1 ]; then tty=(-it); fi
    docker run --rm --init --privileged --name treeman-e2e "${tty[@]}" \
        -e DOCKERHUB_USERNAME -e DOCKERHUB_TOKEN \
        -v "{{justfile_directory()}}:/src:ro" \
        -v treeman-e2e-docker:/var/lib/docker \
        -v treeman-e2e-go:/root/go \
        -v treeman-e2e-gocache:/root/.cache/go-build \
        treeman-e2e-sandbox just _test-e2e-sandboxed "{{batch}}" "{{parallel}}"

[private]
_test-e2e-sandboxed batch parallel:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ "${TREEMAN_E2E_SANDBOX:-}" != "1" ]; then
        echo "_test-e2e-sandboxed runs inside the sandbox only; use 'just test-e2e'" >&2
        exit 2
    fi
    source e2e/sandbox/batches.sh

    pull_retry() {
        local dir=$1 n=0
        while [ "$n" -lt 3 ]; do
            if (cd "$dir" && docker compose pull -q); then return 0; fi
            n=$((n + 1))
            echo "  prepull retry $n/3 for $dir after registry error" >&2
            sleep $((n * 10))
        done
        echo "  WARN: prepull failed for $dir after 3 tries; suite will pull on demand" >&2
    }

    cleanup_e2e_stacks() {
        for d in e2e/*/; do
            [ -f "$d/docker-compose.yml" ] || continue
            (cd "$d" && docker compose down -v --remove-orphans >/dev/null 2>&1) ||
                echo "  WARN: compose down failed in $d" >&2
        done
    }

    run_batch() {
        local name=$1
        local suites=${E2E_BATCHES[$name]:-}
        if [ -z "$suites" ]; then
            if [ "$name" = harness ] || [ "$name" = sandbox ] || [ ! -d "e2e/$name" ]; then
                echo "unknown batch or suite: $name (batches: ${!E2E_BATCHES[*]})" >&2
                exit 2
            fi
            suites=$name
        fi
        local pkgs=() s
        for s in $suites; do
            [ -d "e2e/$s" ] || { echo "batch $name names missing suite e2e/$s" >&2; exit 2; }
            if [ -f "e2e/$s/docker-compose.yml" ]; then pull_retry "e2e/$s"; fi
            pkgs+=("./e2e/$s/...")
        done
        echo
        echo "==================== batch: $name ===================="
        echo "suites: $suites"
        echo "parallel: {{parallel}}"
        go test -tags=e2e -p "{{parallel}}" -timeout 30m "${pkgs[@]}"
        echo "==================== cleanup after $name ===================="
        cleanup_e2e_stacks
    }

    if [ "{{batch}}" = "all" ]; then
        for b in "${E2E_ORDER[@]}"; do run_batch "$b"; done
    else
        run_batch "{{batch}}"
    fi

# List the e2e batch groupings used by `just test-e2e`.
test-e2e-list:
    #!/usr/bin/env bash
    set -euo pipefail
    source e2e/sandbox/batches.sh
    for b in "${E2E_ORDER[@]}" lite; do
        printf '%-9s: %s\n' "$b" "${E2E_BATCHES[$b]}"
    done

check: lint test sync-schema sync-docs sync-flake

# Regenerate the generated docs from the live binary: `docs/cli.md`
# from the cli.Command tree and `docs/config-reference.md` from the
# config.Config types. Catches command/flag/config drift in PRs by
# comparing the rewrite against what's in git. Same pattern as
# sync-schema — cheap enough to run on every `just check`.
sync-docs:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p docs
    go run ./cmd/treeman-gen-docs docs/cli.md
    go run ./cmd/treeman-gen-config-docs docs/config-reference.md
    go run ./cmd/treeman-gen-mcp-docs docs/mcp-tools.md
    go run ./cmd/treeman-gen-events-docs docs/events.md
    go run ./cmd/treeman-gen-frameworks-docs docs/frameworks.md
    go run ./cmd/treeman-gen-rpc-docs docs/rpc-reference.md
    if [ -n "$(git status --porcelain docs/cli.md docs/config-reference.md docs/mcp-tools.md docs/events.md docs/frameworks.md docs/rpc-reference.md)" ]; then
        echo "sync-docs: regenerated generated docs"
    else
        echo "sync-docs: generated docs already in sync"
    fi

# Regenerate `schemas/treeman.schema.json` from the current
# config.Config Go types. Cheap (pure reflection) so we run it on
# every `just check` to keep the canonical schema URL in sync with
# the binary on disk. CI asserts no drift on PRs and auto-commits
# on master pushes.
#
# The canonical schema is REPO-scoped: it validates `.treeman.yaml`
# (the file `treeman init`'s modeline points at via the URL), so it
# excludes global-only keys (daemon, snapshots, logs, status,
# notifications). The global config gets its own scoped schema from
# `treeman schema install --global` / `treeman init --global`.
sync-schema:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p schemas
    go run ./cmd/treeman schema dump --scope repo   --out schemas/treeman.schema.json
    go run ./cmd/treeman schema dump --scope global --out schemas/treeman.global.schema.json
    if [ -n "$(git status --porcelain schemas/treeman.schema.json schemas/treeman.global.schema.json)" ]; then
        echo "sync-schema: regenerated schemas/"
    else
        echo "sync-schema: schemas already in sync"
    fi

# Render the docs site (MkDocs Material, strict: broken links and
# anchors fail) into site/ from freshly regenerated reference pages.
# The pages workflow runs exactly this.
docs-build: sync-docs
    mkdocs build --strict

# Live-reloading preview of the docs site on http://127.0.0.1:8000.
docs-serve: sync-docs
    mkdocs serve

clean:
    rm -rf bin/ site/

# ─────────────────────────── Nix ───────────────────────────

nix-build:
    nix build .#treeman

nix-check:
    nix flake check --print-build-logs

# Keep nix/package.nix's `vendorHash` aligned with the current go.sum.
#
# A sha256 of go.sum is embedded as a `# go-sum:` line in nix/package.nix.
# When the cached digest matches go.sum on disk, sync-flake returns
# immediately without running `nix build`. That makes it cheap
# enough to run on every `just check`, so a dev `go get` flow can
# never push a master commit that breaks nix CI on master.
#
# By default this does NOT touch the version string — release-only
# concern. Pass an explicit `version` argument to also rewrite
# `version = "…"`, which the `-X .../Version=…` ldflag reads (used by
# the release recipes). Pass `--force` to bypass the cache and re-run
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
    CACHED_HASH=$(awk -F': ' '/^[[:space:]]*#[[:space:]]*go-sum:/ {print $2; exit}' nix/package.nix | tr -d ' ')
    CURRENT_VERSION=$(awk -F'"' '/^[[:space:]]*version = "/ {print $2; exit}' nix/package.nix)

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
        sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$SENTINEL"'";|' nix/package.nix
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
        sed -i -E 's|^(\s*vendorHash = )"sha256-[^"]*";|\1"'"$NEW_HASH"'";|' nix/package.nix
        if grep -q '^[[:space:]]*# go-sum:' nix/package.nix; then
            sed -i -E 's|^(\s*# go-sum:).*|\1 '"$GO_SUM_HASH"'|' nix/package.nix
        else
            sed -i -E 's|^(\s*)(vendorHash = )|\1# go-sum: '"$GO_SUM_HASH"'\n\1\2|' nix/package.nix
        fi
        echo "sync-flake: vendorHash=$NEW_HASH go-sum=$GO_SUM_HASH"
    fi

    # Hard guard: refuse to leave the sentinel in nix/package.nix. If we
    # somehow get here with it still present, the working tree is
    # broken and CI will fail on hash mismatch — fail loudly here so
    # the dev catches it before push.
    if grep -q '^\s*vendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="' nix/package.nix; then
        echo "sync-flake: refusing to leave sentinel vendorHash in nix/package.nix" >&2
        exit 1
    fi

    if [ "$NEED_VERSION" = "1" ]; then
        sed -i -E 's|^(\s*version = )"[^"]*";|\1"'"$VERSION"'";|' nix/package.nix
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
    # Always sync nix/package.nix vendorHash + version BEFORE tagging.
    # Even when go.sum hasn't changed, the version string
    # must reflect v${NEW} or `nix profile install` will report a
    # stale version. sync-flake re-validates the build at the end.
    just sync-flake "${NEW}"
    if [ -n "$(git status --porcelain nix/package.nix)" ]; then
        git add nix/package.nix
        git commit -m "chore: bump nix/package.nix to v${NEW}"
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
