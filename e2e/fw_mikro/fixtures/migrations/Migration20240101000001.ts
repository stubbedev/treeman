import { Migration } from "@mikro-orm/migrations";

export class Migration20240101000001 extends Migration {
  async up(): Promise<void> {
    this.addSql(`create table "widget" (
      "id" serial primary key,
      "name" varchar(255) not null,
      "price" decimal(10,2) not null
    );`);
  }
  async down(): Promise<void> {
    this.addSql('drop table "widget";');
  }
}
