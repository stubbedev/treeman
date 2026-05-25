const { pgTable, serial, varchar, decimal } = require("drizzle-orm/pg-core");

exports.widgets = pgTable("widgets", {
  id: serial("id").primaryKey(),
  name: varchar("name", { length: 64 }).notNull(),
  price: decimal("price", { precision: 10, scale: 2 }).notNull(),
});
