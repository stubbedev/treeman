#!/usr/bin/env sh
# Seeds the per-worktree Redis prefix with a fixed set of keys.
# {target_db} (rendered key_prefix_template) is in $REDIS_PREFIX.
set -eu
: "${REDIS_PREFIX:?REDIS_PREFIX not set}"
container="${REDIS_CONTAINER:-treeman-e2e-redis}"

docker exec -i "$container" redis-cli <<EOF
SET ${REDIS_PREFIX}cache:foo "hello"
SET ${REDIS_PREFIX}cache:bar "world"
SADD ${REDIS_PREFIX}users:online "alice" "bob"
EOF
