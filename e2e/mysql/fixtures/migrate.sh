#!/usr/bin/env sh
# Stand-in for a framework's migrate CLI (Laravel, Flyway, Rails, …) —
# applies pending .sql files from fixtures/migrations/ in lexical order
# AND records each application in a `_treeman_e2e_migrations` ledger
# table so re-runs skip the already-applied files. That ledger is what
# makes the incremental-from-ancestor path correct: cloning a cached
# template carries the partial ledger with it, so this script applies
# only the new files on top.
#
# Runs the mysql CLI inside the docker container (the test host
# doesn't necessarily have the client installed). The container is
# guaranteed to be running because treeman's prepare flow connects
# to mysqld before invoking us.
set -eu

: "${DB_DATABASE:?DB_DATABASE not set}"

container="${MYSQL_CONTAINER:-treeman-e2e-mysql}"
migrations_dir="$(dirname "$0")/migrations"

run_sql_stdin() {
  docker exec -i "$container" \
    mysql --user=root --password=rootpw "$DB_DATABASE"
}

# Ledger table: stores one row per applied filename. CREATE IF NOT
# EXISTS is idempotent across cold + incremental runs.
printf '%s\n' \
  "CREATE TABLE IF NOT EXISTS _treeman_e2e_migrations (filename VARCHAR(255) PRIMARY KEY);" \
  | run_sql_stdin

for f in "$migrations_dir"/*.sql; do
  base="$(basename "$f")"
  count=$(docker exec -i "$container" mysql --user=root --password=rootpw \
    --batch --skip-column-names "$DB_DATABASE" <<EOF
SELECT COUNT(*) FROM _treeman_e2e_migrations WHERE filename = '$base';
EOF
  )
  if [ "$count" != "0" ]; then
    echo "skipping $base (already applied)" >&2
    continue
  fi
  echo "applying $base" >&2
  docker exec -i "$container" \
    mysql --user=root --password=rootpw "$DB_DATABASE" < "$f"
  printf "INSERT INTO _treeman_e2e_migrations(filename) VALUES ('%s');\n" "$base" \
    | run_sql_stdin
done
