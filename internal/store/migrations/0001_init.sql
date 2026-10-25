-- The users table is shared with the workout app, which owns it (see
-- usersDDL in store.go for the agreed shape). Everything this app owns lives
-- in the food schema; the workout app reads from it and never writes to it.

CREATE TABLE food.sessions (
  id           text PRIMARY KEY,
  user_id      bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  created_at   timestamptz NOT NULL,
  expires_at   timestamptz NOT NULL,
  last_seen_at timestamptz NOT NULL
);
CREATE INDEX food_sessions_user ON food.sessions (user_id);

-- Shared product master keyed by barcode. A NULL nutrient means "unknown", never 0.
CREATE TABLE food.products (
  barcode    text PRIMARY KEY,
  name       text NOT NULL,
  brand      text NOT NULL DEFAULT '',
  unit       text NOT NULL,
  kcal       double precision,
  protein_g  double precision,
  fat_g      double precision,
  carbs_g    double precision,
  salt_g     double precision,
  source     text NOT NULL DEFAULT 'manual',
  updated_by bigint REFERENCES users (id) ON DELETE SET NULL,
  updated_at timestamptz NOT NULL
);

-- Nutrition is copied into each log row at logging time, so editing a product
-- later never rewrites what was already eaten. eaten_on is the phone's local
-- date; eaten_at is the UTC instant.
CREATE TABLE food.logs (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id    bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  barcode    text,
  name       text NOT NULL,
  unit       text NOT NULL,
  servings   double precision NOT NULL,
  eaten_on   date NOT NULL,
  eaten_at   timestamptz NOT NULL,
  kcal       double precision,
  protein_g  double precision,
  fat_g      double precision,
  carbs_g    double precision,
  salt_g     double precision,
  created_at timestamptz NOT NULL,
  deleted_at timestamptz
);
CREATE INDEX food_logs_user_day ON food.logs (user_id, eaten_on);
