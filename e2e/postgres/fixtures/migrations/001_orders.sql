CREATE TABLE orders (
  id SERIAL PRIMARY KEY,
  product_id INT NOT NULL REFERENCES products(id),
  qty INT NOT NULL
);
