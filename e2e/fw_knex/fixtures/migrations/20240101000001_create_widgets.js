exports.up = function (knex) {
  return knex.schema.createTable("widgets", (t) => {
    t.increments("id").primary();
    t.string("name").notNullable();
    t.decimal("price", 10, 2).notNullable();
  });
};
exports.down = function (knex) {
  return knex.schema.dropTable("widgets");
};
