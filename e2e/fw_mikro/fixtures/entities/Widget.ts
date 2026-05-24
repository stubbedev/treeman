import { Entity, PrimaryKey, Property } from "@mikro-orm/core";

@Entity()
export class Widget {
  @PrimaryKey()
  id!: number;

  @Property()
  name!: string;

  @Property({ columnType: "decimal(10,2)" })
  price!: string;
}
