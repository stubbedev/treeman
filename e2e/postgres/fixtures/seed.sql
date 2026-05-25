CREATE TABLE products (
  id SERIAL PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  price NUMERIC(10, 2) NOT NULL
);

INSERT INTO products (name, price) VALUES
  ('Widget', 9.99),
  ('Gadget', 19.99),
  ('Doohickey', 29.99);
