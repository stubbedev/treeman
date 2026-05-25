-- Seed dump loaded into the source DB before migrations run.
-- mysqldump-style: USE statement is stripped by the loader; we land
-- in whatever DB the connection is scoped to (the per-worktree
-- name_template-rendered DB).
CREATE TABLE products (
  id INT PRIMARY KEY AUTO_INCREMENT,
  name VARCHAR(255) NOT NULL,
  price DECIMAL(10, 2) NOT NULL
) ENGINE=InnoDB;

INSERT INTO products (name, price) VALUES
  ('Widget', 9.99),
  ('Gadget', 19.99),
  ('Doohickey', 29.99);
