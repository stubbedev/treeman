#!/usr/bin/env sh
# Stand-in for a framework's migrate CLI. Applies pending .sql files
# from fixtures/migrations/ in lexical order AND records each
# application in a `_treeman_e2e_migrations` ledger so re-runs skip
# the already-applied files. That ledger is what makes the
# incremental-from-ancestor path correct: cloning a cached template
# carries the partial ledger with it, so this script applies only the
# new files on top.
set -eu
: "${DB_DATABASE:?DB_DATABASE not set}"
container="${PG_CONTAINER:-treeman-e2e-postgres}"
dir="$(dirname "$0")/migrations"

psql_in() {
  docker exec -i -e PGPASSWORD=pgpw "$container" \
    psql -U postgres -d "$DB_DATABASE" -v ON_ERROR_STOP=1
}

# Ledger table. CREATE IF NOT EXISTS is idempotent across cold +
# incremental runs.
printf '%s\n' \
  "CREATE TABLE IF NOT EXISTS _treeman_e2e_migrations (filename TEXT PRIMARY KEY);" \
  | psql_in

for f in "$dir"/*.sql; do
  base="$(basename "$f")"
  applied=$(docker exec -i -e PGPASSWORD=pgpw "$container" \
    psql -U postgres -d "$DB_DATABASE" -At -v ON_ERROR_STOP=1 \
    -c "SELECT 1 FROM _treeman_e2e_migrations WHERE filename = '$base'" || true)
  if [ "$applied" = "1" ]; then
    echo "skipping $base (already applied)" >&2
    continue
  fi
  echo "applying $base" >&2
  docker exec -i -e PGPASSWORD=pgpw "$container" \
    psql -U postgres -d "$DB_DATABASE" -v ON_ERROR_STOP=1 < "$f"
  printf "INSERT INTO _treeman_e2e_migrations(filename) VALUES ('%s');\n" "$base" \
    | psql_in
done
