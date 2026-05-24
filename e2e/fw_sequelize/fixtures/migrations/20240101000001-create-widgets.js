"use strict";
module.exports = {
  async up(q, S) {
    await q.createTable("widgets", {
      id: { type: S.INTEGER, primaryKey: true, autoIncrement: true },
      name: { type: S.STRING, allowNull: false },
      price: { type: S.DECIMAL(10, 2), allowNull: false },
    });
  },
  async down(q) {
    await q.dropTable("widgets");
  },
};
