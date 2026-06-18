#!/usr/bin/env sh
# Seeds a per-worktree key prefix with a fixed key set, proving the
# redis-family driver drives Valkey and DragonflyDB unchanged.
#
# {target_db} (the rendered key prefix) arrives in $REDIS_PREFIX.
# EXEC_CONTAINER is the container we shell into for a redis-cli — Valkey
# ships one, so it serves as the client box for both engines. When
# TARGET_HOST is set the keys are written to that compose-network host
# instead (used to reach DragonflyDB, which has no in-image CLI).
set -eu
: "${REDIS_PREFIX:?REDIS_PREFIX not set}"
exec_container="${EXEC_CONTAINER:-treeman-e2e-valkey}"

host_args=""
if [ -n "${TARGET_HOST:-}" ]; then
	host_args="-h ${TARGET_HOST} -p ${TARGET_PORT:-6379}"
fi

# shellcheck disable=SC2086  # host_args is intentionally word-split
docker exec -i "$exec_container" redis-cli $host_args <<EOF
SET ${REDIS_PREFIX}cache:foo "hello"
SET ${REDIS_PREFIX}cache:bar "world"
SADD ${REDIS_PREFIX}users:online "alice" "bob"
EOF
