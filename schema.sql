CREATE TABLE review (
  id SERIAL PRIMARY KEY,
  platform VARCHAR(8) NOT NULL,
  author VARCHAR(255) NULL,
  author_uri VARCHAR(255) NULL,
  updated_at DATE NOT NULL
);
CREATE INDEX author_uri_idx on review(author_uri);
CREATE INDEX platform_idx on review(platform);
