CREATE TABLE IF NOT EXISTS cost_hours (
 hour timestamptz PRIMARY KEY,
 rows jsonb NOT NULL,
 observed_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS cost_days (
 day date PRIMARY KEY,
 rows jsonb NOT NULL,
 sampled_hours integer NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS cost_exports (
 day date PRIMARY KEY,
 exported_at timestamptz NOT NULL DEFAULT now()
);
