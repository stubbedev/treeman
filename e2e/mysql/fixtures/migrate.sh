#!/usr/bin/env sh
# Stand-in for a framework's migrate CLI. Applies every .sql file in
# fixtures/migrations/ in lexical order against the target DB.
#
# Runs the mysql CLI inside the docker container (the test host
# doesn't necessarily have the client installed). The container is
# guaranteed to be running because treeman's prepare flow connects
# to mysqld before invoking us.
set -eu

: "${DB_DATABASE:?DB_DATABASE not set}"

container="${MYSQL_CONTAINER:-treeman-e2e-mysql}"
migrations_dir="$(dirname "$0")/migrations"

for f in "$migrations_dir"/*.sql; do
  echo "applying $(basename "$f")" >&2
  docker exec -i "$container" \
    mysql --user=root --password=rootpw "$DB_DATABASE" < "$f"
done
