#!/usr/bin/env sh
# Seed the target Mongo DB by running a small mongosh script inside
# the container. {target_db} is substituted by treeman.
set -eu
: "${MONGO_DB:?MONGO_DB not set}"
container="${MONGO_CONTAINER:-treeman-e2e-mongo}"

docker exec -i "$container" mongosh --quiet --eval "
db = db.getSiblingDB('$MONGO_DB');
db.products.deleteMany({});
db.products.insertMany([
  {name: 'Widget', price: 9.99},
  {name: 'Gadget', price: 19.99},
  {name: 'Doohickey', price: 29.99}
]);
db.orders.deleteMany({});
db.orders.insertOne({product: 'Widget', qty: 5});
"
