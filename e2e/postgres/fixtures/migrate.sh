#!/usr/bin/env sh
set -eu
: "${DB_DATABASE:?DB_DATABASE not set}"
container="${PG_CONTAINER:-treeman-e2e-postgres}"
dir="$(dirname "$0")/migrations"
for f in "$dir"/*.sql; do
  echo "applying $(basename "$f")" >&2
  docker exec -i -e PGPASSWORD=pgpw "$container" \
    psql -U postgres -d "$DB_DATABASE" -v ON_ERROR_STOP=1 < "$f"
done
