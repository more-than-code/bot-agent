-- Procstat, systemd unit, and HTTP hypertables, plus the service registry.
--
-- Run on pi4, database cluster_metrics, as a superuser. Idempotent: safe to re-run.
-- Do not run from an agent session. Writers are insert-only (create_templates = []),
-- so this file creates the tables before Telegraf can insert.
--
-- Roles that must already exist: metrics_owner, metrics_writer, metrics_reader.
-- metrics_grokbox and metrics_pi1..metrics_pi4 are members of metrics_writer and
-- inherit INSERT. A second GRANT to metrics_grokbox is not needed.
-- TimescaleDB is already enabled in this database.
--
-- metrics.procstat already exists on pi4 (empty, owner metrics_owner, 7-day chunks,
-- an older column set, no retention policy). This file does not drop or retype those
-- columns. It adds any missing columns (including filter and cmdline), sets the
-- chunk interval to 1 day, and adds a 7-day retention policy. No other raw metrics
-- table has a retention policy. The 7-day policy is new for this table, and for
-- systemd_units and http_response. Once data is older than 7 days, Timescale drops
-- those chunks. That is the destructive part of this file. The table is empty today.
--
-- Columns this snippet writes (tags text, floats double precision, ints bigint):
--   tags: host, process_name, pid, user, cmdline, filter, systemd_unit
--   fields: cpu_usage, memory_rss (numeric; Telegraf sends uint64), memory_usage,
--           num_threads, created_at (nanoseconds)
-- timestamp without time zone, UTC, same as metrics.cpu. The live table also has
-- older columns (pattern, exe, cpu_time_*, memory_vms, num_fds, read_bytes, ...).
-- They stay. The snippet's taginclude / fieldpass does not write them.
--
-- metrics.systemd_units and metrics.http_response do not exist. Telegraf will not
-- create them. details = false on systemd_units, so the extra detail fields are
-- not sent. http_response on Telegraf 1.40.1 also sends result_type (text).
--
-- metrics.services is the registry, not a Telegraf measurement. The seed is the
-- manifests in services/*.json. Re-running updates them and deletes registry names
-- that are no longer in the seed. Writers do not get INSERT.
--
-- Rollback, only if Joe asks. DROP of procstat removes the existing empty table:
--   SELECT remove_retention_policy('metrics.procstat', if_exists => TRUE);
--   SELECT remove_retention_policy('metrics.systemd_units', if_exists => TRUE);
--   SELECT remove_retention_policy('metrics.http_response', if_exists => TRUE);
--   DROP TABLE IF EXISTS metrics.http_response;
--   DROP TABLE IF EXISTS metrics.systemd_units;
--   DROP TABLE IF EXISTS metrics.procstat;
--   DROP TABLE IF EXISTS metrics.services;

CREATE SCHEMA IF NOT EXISTS metrics;

CREATE TABLE IF NOT EXISTS metrics.procstat (
  time timestamp without time zone NOT NULL,
  host text,
  process_name text,
  pid text,
  "user" text,
  cmdline text,
  filter text,
  systemd_unit text,
  cpu_usage double precision,
  memory_rss numeric,
  memory_usage double precision,
  num_threads bigint,
  created_at bigint
);

ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS host text;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS process_name text;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS pid text;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS "user" text;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS cmdline text;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS filter text;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS systemd_unit text;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS cpu_usage double precision;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS memory_rss numeric;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS memory_usage double precision;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS num_threads bigint;
ALTER TABLE metrics.procstat ADD COLUMN IF NOT EXISTS created_at bigint;

ALTER TABLE metrics.procstat OWNER TO metrics_owner;

SELECT create_hypertable(
  'metrics.procstat',
  'time',
  chunk_time_interval => INTERVAL '1 day',
  if_not_exists => TRUE,
  migrate_data => TRUE
);

-- The live hypertable was created with 7-day chunks. if_not_exists does not change that.
SELECT set_chunk_time_interval('metrics.procstat', INTERVAL '1 day');

CREATE INDEX IF NOT EXISTS procstat_host_time_idx
  ON metrics.procstat (host, time DESC);

SELECT add_retention_policy(
  'metrics.procstat',
  INTERVAL '7 days',
  if_not_exists => TRUE
);

GRANT INSERT ON TABLE metrics.procstat TO metrics_writer;
GRANT SELECT ON TABLE metrics.procstat TO metrics_reader;

-- System-scope units only. User units are not collected (wrong bus).
CREATE TABLE IF NOT EXISTS metrics.systemd_units (
  time timestamp without time zone NOT NULL,
  host text,
  name text,
  load text,
  active text,
  sub text,
  load_code bigint,
  active_code bigint,
  sub_code bigint
);

ALTER TABLE metrics.systemd_units OWNER TO metrics_owner;

SELECT create_hypertable(
  'metrics.systemd_units',
  'time',
  chunk_time_interval => INTERVAL '1 day',
  if_not_exists => TRUE,
  migrate_data => TRUE
);

CREATE INDEX IF NOT EXISTS systemd_units_host_name_time_idx
  ON metrics.systemd_units (host, name, time DESC);

SELECT add_retention_policy(
  'metrics.systemd_units',
  INTERVAL '7 days',
  if_not_exists => TRUE
);

GRANT INSERT ON TABLE metrics.systemd_units TO metrics_writer;
GRANT SELECT ON TABLE metrics.systemd_units TO metrics_reader;

-- status_code is a tag (text). http_response_code is the integer field.
-- result_type repeats the result tag; Telegraf 1.40.1 sends it on every sample.
CREATE TABLE IF NOT EXISTS metrics.http_response (
  time timestamp without time zone NOT NULL,
  host text,
  server text,
  method text,
  status_code text,
  result text,
  response_time double precision,
  http_response_code bigint,
  result_code bigint,
  result_type text,
  content_length bigint
);

ALTER TABLE metrics.http_response OWNER TO metrics_owner;

SELECT create_hypertable(
  'metrics.http_response',
  'time',
  chunk_time_interval => INTERVAL '1 day',
  if_not_exists => TRUE,
  migrate_data => TRUE
);

CREATE INDEX IF NOT EXISTS http_response_host_server_time_idx
  ON metrics.http_response (host, server, time DESC);

SELECT add_retention_policy(
  'metrics.http_response',
  INTERVAL '7 days',
  if_not_exists => TRUE
);

GRANT INSERT ON TABLE metrics.http_response TO metrics_writer;
GRANT SELECT ON TABLE metrics.http_response TO metrics_reader;

CREATE TABLE IF NOT EXISTS metrics.services (
  name text PRIMARY KEY,
  project text NOT NULL,
  host text NOT NULL,
  port integer,
  health_url text,
  systemd_unit text,
  systemd_scope text,
  process_match text,
  updated_at timestamp without time zone NOT NULL DEFAULT (now() AT TIME ZONE 'UTC'),
  CONSTRAINT services_port_range CHECK (port IS NULL OR port BETWEEN 1 AND 65535),
  CONSTRAINT services_scope CHECK (systemd_scope IS NULL OR systemd_scope IN ('system', 'user'))
);

ALTER TABLE metrics.services OWNER TO metrics_owner;
GRANT SELECT ON TABLE metrics.services TO metrics_reader;

-- Hosts and units checked read-only on 2026-10-12. Do not treat these as assumptions.
--   pi1 user units (uid 1000, app.slice): tra-backend :3031, tra-webapp :5176,
--     ttd-backend :3001, ttd-webapp :5173.
--   pi1 tmux, no unit: bfd-backend :3021 /health, bfd-webapp :5174.
--   pi2: user unit bot-console :5180.
--   pi3: no monitored services (bot_agent retired 2026-10-12).
--   pi4: postgresql@18-main.service :5432 (postgresql.service is an exited wrapper);
--     pgadmin4 hand-started on :5050 (no unit).
-- User-unit health is HTTP plus process presence. The unit name is only the cgroup.
-- bot-console's cmdline is `node build` (working directory is the workspace), so it
-- has no process_match. The cgroup filter identifies it.

INSERT INTO metrics.services (
  name, project, host, port, health_url, systemd_unit, systemd_scope, process_match
) VALUES
  ('bfd-backend', 'briefed', 'pi1', 3021, 'http://127.0.0.1:3021/health', NULL, NULL, '/home/joe/workspace/briefed/bfd-backend'),
  ('bfd-webapp', 'briefed', 'pi1', 5174, 'http://127.0.0.1:5174/', NULL, NULL, '/home/joe/workspace/briefed/bfd-webapp'),
  ('bot-console', 'deploybot', 'pi2', 5180, 'http://127.0.0.1:5180/', 'bot-console.service', 'user', NULL),
  ('pgadmin', 'pgadmin', 'pi4', 5050, 'http://127.0.0.1:5050/', NULL, NULL, 'pgadmin4'),
  ('postgres', 'postgres', 'pi4', 5432, NULL, 'postgresql@18-main.service', 'system', 'postgres'),
  ('tra-backend', 'tour-agent', 'pi1', 3031, 'http://127.0.0.1:3031/health', 'tra-backend.service', 'user', '/home/joe/workspace/tour-agent/tra-backend'),
  ('tra-webapp', 'tour-agent', 'pi1', 5176, 'http://127.0.0.1:5176/', 'tra-webapp.service', 'user', '/home/joe/workspace/tour-agent/tra-webapp'),
  ('ttd-backend', 'tutored', 'pi1', 3001, 'http://127.0.0.1:3001/health', 'ttd-backend.service', 'user', '/home/joe/workspace/tutored/ttd-backend'),
  ('ttd-webapp', 'tutored', 'pi1', 5173, 'http://127.0.0.1:5173/', 'ttd-webapp.service', 'user', '/home/joe/workspace/tutored/ttd-webapp')
ON CONFLICT (name) DO UPDATE SET
  project = EXCLUDED.project,
  host = EXCLUDED.host,
  port = EXCLUDED.port,
  health_url = EXCLUDED.health_url,
  systemd_unit = EXCLUDED.systemd_unit,
  systemd_scope = EXCLUDED.systemd_scope,
  process_match = EXCLUDED.process_match,
  updated_at = (now() AT TIME ZONE 'UTC');

-- Drops registry rows that are no longer in the manifests (the old briefed row).
DELETE FROM metrics.services
WHERE name NOT IN (
  'bfd-backend',
  'bfd-webapp',
  'bot-console',
  'pgadmin',
  'postgres',
  'tra-backend',
  'tra-webapp',
  'ttd-backend',
  'ttd-webapp'
);
