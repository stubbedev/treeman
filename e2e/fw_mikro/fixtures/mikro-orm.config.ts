import { defineConfig } from "@mikro-orm/postgresql";
import { Widget } from "./entities/Widget";
import { Migrator } from "@mikro-orm/migrations";

export default defineConfig({
  clientUrl: process.env.DATABASE_URL,
  entities: [Widget],
  extensions: [Migrator],
  migrations: { path: "./migrations" },
});
