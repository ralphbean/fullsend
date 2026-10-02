CREATE TABLE IF NOT EXISTS schema_versions (version integer PRIMARY KEY);
CREATE TABLE IF NOT EXISTS config_revisions (
  digest text PRIMARY KEY,
  content jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS source_checkpoints (
  tenant text NOT NULL,
  source text NOT NULL,
  cursor text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, source)
);
CREATE TABLE IF NOT EXISTS observations (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tenant text NOT NULL,
  source text NOT NULL,
  observation_key text NOT NULL,
  entity text NOT NULL,
  revision text NOT NULL,
  kind text NOT NULL,
  behavior text NOT NULL,
  authorized boolean NOT NULL,
  config_digest text NOT NULL REFERENCES config_revisions(digest),
  selection_state text NOT NULL DEFAULT 'pending',
  selection_error text NOT NULL DEFAULT '',
  UNIQUE (tenant, source, observation_key)
);
CREATE TABLE IF NOT EXISTS selected_runs (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  observation_id bigint NOT NULL REFERENCES observations(id),
  harness text NOT NULL,
  state text NOT NULL DEFAULT 'waiting',
  UNIQUE (observation_id, harness)
);
CREATE TABLE IF NOT EXISTS attempts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  run_id bigint NOT NULL REFERENCES selected_runs(id),
  number integer NOT NULL,
  job_name text NOT NULL UNIQUE,
  job_uid text NOT NULL DEFAULT '',
  state text NOT NULL DEFAULT 'creating',
  outcome text NOT NULL DEFAULT '',
  cleanup_state text NOT NULL DEFAULT 'pending',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (run_id, number)
);
CREATE INDEX IF NOT EXISTS selected_runs_state_idx ON selected_runs(state);
CREATE INDEX IF NOT EXISTS attempts_state_idx ON attempts(state);
