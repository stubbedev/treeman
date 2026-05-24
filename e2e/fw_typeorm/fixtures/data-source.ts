import "reflect-metadata";
import { DataSource } from "typeorm";

export const AppDataSource = new DataSource({
  type: "postgres",
  url: process.env.DATABASE_URL,
  entities: [__dirname + "/entities/*.ts"],
  migrations: [__dirname + "/migrations/*.ts"],
  synchronize: false,
  logging: false,
});
