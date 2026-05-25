import { MigrationInterface, QueryRunner, Table } from "typeorm";

export class CreateWidget1700000000000 implements MigrationInterface {
  async up(q: QueryRunner): Promise<void> {
    await q.createTable(
      new Table({
        name: "widget",
        columns: [
          { name: "id", type: "int", isPrimary: true, isGenerated: true, generationStrategy: "increment" },
          { name: "name", type: "varchar" },
          { name: "price", type: "numeric", precision: 10, scale: 2 },
        ],
      }),
    );
  }
  async down(q: QueryRunner): Promise<void> {
    await q.dropTable("widget");
  }
}
