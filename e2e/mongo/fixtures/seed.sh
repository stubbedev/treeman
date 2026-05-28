#!/usr/bin/env sh
# Seed the target Mongo DB by running a small mongosh script inside the
# container. {target_db} is substituted by treeman via $MONGO_DB.
#
# The insert is retried + verified: a fresh mongod can accept a TCP
# connection (and pass a ping healthcheck) a beat before it is fully
# ready to durably serve writes, especially under the disk/daemon
# contention of several engine stacks booting at once. Rather than
# silently leave 0 documents (which surfaces far away as a confusing
# count assertion), we loop until countDocuments() confirms the write
# landed, and exit non-zero — loudly failing the prepare — if it never
# does.
set -eu
: "${MONGO_DB:?MONGO_DB not set}"
container="${MONGO_CONTAINER:-treeman-e2e-mongo}"

i=0
count=""
while [ "$i" -lt 30 ]; do
  count=$(docker exec -i "$container" mongosh --quiet --eval "
db = db.getSiblingDB('$MONGO_DB');
db.products.deleteMany({});
db.products.insertMany([
  {name: 'Widget', price: 9.99},
  {name: 'Gadget', price: 19.99},
  {name: 'Doohickey', price: 29.99}
]);
db.orders.deleteMany({});
db.orders.insertOne({product: 'Widget', qty: 5});
print(db.products.countDocuments({}));
" 2>/dev/null | tr -d '[:space:]') || count=""
  if [ "$count" = "3" ]; then
    exit 0
  fi
  i=$((i + 1))
  sleep 1
done

echo "mongo seed: products countDocuments != 3 after 30 attempts (last='${count}')" >&2
exit 1
