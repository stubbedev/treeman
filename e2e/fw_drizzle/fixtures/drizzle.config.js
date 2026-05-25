module.exports = {
  schema: "./schema.js",
  out: "./migrations",
  dialect: "postgresql",
  dbCredentials: { url: process.env.DATABASE_URL },
};
