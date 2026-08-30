-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- VectoraDB Schema Ledger (RECORD layer) — a record the database keeps about
-- itself. Event triggers capture every DDL change, attribute it (human vs
-- agent, tool, session, branch), and enforce guardrails on destructive DDL.
--
-- The ledger lives in schema "vdb" (deliberately NOT "vectoradb", which is the
-- role name and therefore the "$user" default schema — an unqualified user table
-- must land in public and be captured, not hidden inside our own schema).
--
-- Idempotent: event triggers are dropped first so re-installing never fires them
-- on its own DDL. Installed into `main`; inherited by every ZFS-clone branch,
-- each keeping its own per-branch history.

DROP EVENT TRIGGER IF EXISTS vdb_guard_start;
DROP EVENT TRIGGER IF EXISTS vdb_log_end;
DROP EVENT TRIGGER IF EXISTS vdb_log_drop;

CREATE EXTENSION IF NOT EXISTS dblink;
CREATE SCHEMA IF NOT EXISTS vdb;

CREATE TABLE IF NOT EXISTS vdb.schema_ledger (
  id              bigserial PRIMARY KEY,
  at              timestamptz NOT NULL DEFAULT clock_timestamp(),
  actor           text,          -- e.g. 'priya' or 'agent-alice'
  actor_kind      text,          -- 'human' | 'agent'
  tool            text,          -- application_name, e.g. 'cursor/opus', 'psql'
  session         text,          -- gateway session id / backend pid
  branch          text,          -- routing branch name
  command_tag     text,          -- 'CREATE INDEX', 'ALTER TABLE', 'DROP TABLE'
  object_type     text,          -- 'table', 'index', ...
  object_identity text,          -- 'public.orders'
  statement       text,          -- the SQL (current_query)
  status          text NOT NULL, -- 'APPLIED' | 'FLAGGED' | 'BLOCKED'
  risk            text,          -- 'drop' | 'type-change' | 'policy' | NULL
  prev_hash       text,          -- row_hash of the previous ledger row
  row_hash        text           -- sha256 over this row's fields, chaining prev_hash
);
-- Add the chain columns to a ledger created before tamper-evidence existed.
ALTER TABLE vdb.schema_ledger ADD COLUMN IF NOT EXISTS prev_hash text;
ALTER TABLE vdb.schema_ledger ADD COLUMN IF NOT EXISTS row_hash  text;
CREATE INDEX IF NOT EXISTS schema_ledger_at_idx ON vdb.schema_ledger (at DESC);

-- Guardrail policy: op (command tag) -> action ('block' | 'flag' | 'allow').
CREATE TABLE IF NOT EXISTS vdb.policy (op text PRIMARY KEY, action text NOT NULL);
INSERT INTO vdb.policy(op, action) VALUES
  ('DROP TABLE',  'block'),
  ('DROP SCHEMA', 'block')
ON CONFLICT (op) DO NOTHING;

-- Attribution context. The actor is the login identity (session_user) when the
-- client logged in as a per-user role — a value a client CANNOT change (SET ROLE
-- leaves session_user untouched), so gateway attribution is non-forgeable. The
-- shared/admin roles fall back to the gateway-injected actor. The rest is read
-- from connection settings the Gateway injects.
CREATE OR REPLACE FUNCTION vdb._ctx(
  OUT actor text, OUT actor_kind text, OUT tool text, OUT session text, OUT branch text
) LANGUAGE sql STABLE AS $$
  SELECT CASE WHEN session_user NOT IN ('vectoradb','vdbclient')
              THEN session_user
              ELSE current_setting('vdb.actor', true) END,
         coalesce(nullif(current_setting('vdb.actor_kind', true), ''), 'human'),
         nullif(current_setting('application_name', true), ''),
         coalesce(nullif(current_setting('vdb.session', true), ''), pg_backend_pid()::text),
         current_setting('vdb.branch', true);
$$;

-- Should this DDL be recorded, or is it internal noise?
CREATE OR REPLACE FUNCTION vdb._skip(schema_name text, command_tag text)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
  -- GRANT/REVOKE and function DDL are recorded (a function that reads sensitive
  -- data, or a privilege change, is exactly what an audit wants). Only the
  -- ledger's own plumbing and pure-noise tags are skipped.
  SELECT schema_name = 'vdb'   -- the ledger's own objects
      OR command_tag IN ('ALTER DATABASE','COMMENT','CREATE EXTENSION',
                         'CREATE EVENT TRIGGER','DROP EVENT TRIGGER','ALTER EVENT TRIGGER',
                         'CREATE SCHEMA');
$$;

-- ddl_command_start: enforce guardrails BEFORE the command runs.
-- SECURITY DEFINER: the ledger's triggers record history with the owner's
-- privileges, so a non-superuser client can trigger them (by running DDL) without
-- being granted any write access to the ledger itself. search_path is pinned.
CREATE OR REPLACE FUNCTION vdb.guard_ddl_start() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, vdb AS $$
DECLARE act text; c record; allow text;
BEGIN
  SELECT action INTO act FROM vdb.policy WHERE op = TG_TAG;
  IF act IS DISTINCT FROM 'block' THEN
    RETURN; -- allowed (or flagged) — recorded in the end/drop triggers
  END IF;
  allow := coalesce(nullif(current_setting('vdb.allow_destructive', true), ''), 'off');
  IF allow IN ('on','true','1') THEN
    RETURN; -- approved override
  END IF;
  SELECT * INTO c FROM vdb._ctx();
  -- Record the blocked attempt durably via dblink (autonomous — survives the rollback).
  PERFORM dblink_exec(
    'host=/var/run/postgresql dbname=' || current_database() || ' user=' || current_user,
    format($f$INSERT INTO vdb.schema_ledger
            (actor,actor_kind,tool,session,branch,command_tag,statement,status,risk)
            VALUES (%L,%L,%L,%L,%L,%L,%L,'BLOCKED','policy')$f$,
      c.actor, c.actor_kind, c.tool, c.session, c.branch, TG_TAG, current_query()));
  RAISE EXCEPTION 'VectoraDB guardrail: % is blocked by policy (set vdb.allow_destructive=on to override)', TG_TAG
    USING ERRCODE = 'insufficient_privilege';
END;
$$;

-- ddl_command_end: record CREATE/ALTER changes (drops are recorded in sql_drop).
CREATE OR REPLACE FUNCTION vdb.log_ddl_end() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, vdb AS $$
DECLARE r record; c record; q text; st text; rk text;
BEGIN
  SELECT * INTO c FROM vdb._ctx();
  q := current_query();
  FOR r IN SELECT * FROM pg_event_trigger_ddl_commands() LOOP
    IF r.command_tag LIKE 'DROP%' THEN CONTINUE; END IF;
    IF vdb._skip(r.schema_name, r.command_tag) THEN CONTINUE; END IF;
    -- Skip `serial`/`primary key` byproducts so one user statement = one row:
    -- the auto sequence, its ALTER SEQUENCE OWNED BY, and the pkey index.
    IF r.command_tag IN ('CREATE SEQUENCE','ALTER SEQUENCE') THEN CONTINUE; END IF;
    IF r.command_tag = 'CREATE INDEX' AND r.object_identity ~ '_pkey$' THEN CONTINUE; END IF;
    st := 'APPLIED'; rk := NULL;
    IF r.command_tag = 'ALTER TABLE' AND q ~* '\malter\M[^;]*\mtype\M' THEN
      st := 'FLAGGED'; rk := 'type-change';
    ELSIF r.command_tag = 'ALTER TABLE' AND q ~* 'drop\s+column' THEN
      st := 'FLAGGED'; rk := 'drop-column';
    ELSIF r.command_tag IN ('CREATE FUNCTION','ALTER FUNCTION') AND q ~* 'security\s+definer' THEN
      st := 'FLAGGED'; rk := 'security-definer';
    END IF;
    INSERT INTO vdb.schema_ledger
      (actor,actor_kind,tool,session,branch,command_tag,object_type,object_identity,statement,status,risk)
    VALUES (c.actor,c.actor_kind,c.tool,c.session,c.branch,
            r.command_tag,r.object_type,r.object_identity,q,st,rk);
  END LOOP;
END;
$$;

-- sql_drop: record drops that were allowed through the guardrails.
CREATE OR REPLACE FUNCTION vdb.log_ddl_drop() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, vdb AS $$
DECLARE r record; c record; q text;
BEGIN
  SELECT * INTO c FROM vdb._ctx();
  q := current_query();
  -- `original` = the object explicitly named in the command (not cascade artifacts
  -- like the sequence, primary-key index, or toast table).
  FOR r IN SELECT * FROM pg_event_trigger_dropped_objects() WHERE NOT is_temporary AND original LOOP
    IF vdb._skip(r.schema_name, TG_TAG) THEN CONTINUE; END IF;
    IF r.object_type NOT IN ('table','view','index','sequence','schema','type','materialized view','function') THEN
      CONTINUE;
    END IF;
    INSERT INTO vdb.schema_ledger
      (actor,actor_kind,tool,session,branch,command_tag,object_type,object_identity,statement,status,risk)
    VALUES (c.actor,c.actor_kind,c.tool,c.session,c.branch,
            TG_TAG,r.object_type,r.object_identity,q,'APPLIED','drop');
  END LOOP;
END;
$$;

-- ── Tamper-evidence ─────────────────────────────────────────────────────────
-- Every row is hash-chained to the one before it: row_hash = sha256(prev_hash ||
-- this row's fields). Deleting or editing a row breaks every hash after it, and
-- `vdb ledger verify` recomputes the chain to catch it. Detection holds no matter
-- who did it; append-only enforcement (below) blocks the ordinary path. Full
-- prevention against a superuser needs the non-superuser app role (roadmap).

-- _ledger_hash is the single source of truth for a row's hash, used by both the
-- insert trigger and verification, so the two can never drift. `at` is rendered
-- in UTC so the hash is independent of the verifying session's timezone.
CREATE OR REPLACE FUNCTION vdb._ledger_hash(r vdb.schema_ledger) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
  SELECT encode(sha256(convert_to(
    coalesce(r.prev_hash,'')      || '|' || coalesce(r.id::text,'')              || '|' ||
    coalesce((r.at AT TIME ZONE 'UTC')::text,'')                                 || '|' ||
    coalesce(r.actor,'')          || '|' || coalesce(r.actor_kind,'')            || '|' ||
    coalesce(r.tool,'')           || '|' || coalesce(r.session,'')               || '|' ||
    coalesce(r.branch,'')         || '|' || coalesce(r.command_tag,'')           || '|' ||
    coalesce(r.object_type,'')    || '|' || coalesce(r.object_identity,'')       || '|' ||
    coalesce(r.statement,'')      || '|' || coalesce(r.status,'')                || '|' ||
    coalesce(r.risk,''), 'UTF8')), 'hex');
$$;

CREATE OR REPLACE FUNCTION vdb.chain_row() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE prev text;
BEGIN
  -- Serialize appends so two concurrent inserts can't chain off the same row.
  PERFORM pg_advisory_xact_lock(hashtext('vdb.schema_ledger'));
  SELECT row_hash INTO prev FROM vdb.schema_ledger ORDER BY id DESC LIMIT 1;
  NEW.prev_hash := coalesce(prev, '');
  NEW.row_hash  := vdb._ledger_hash(NEW);
  RETURN NEW;
END;
$$;
CREATE OR REPLACE TRIGGER vdb_chain BEFORE INSERT ON vdb.schema_ledger
  FOR EACH ROW EXECUTE FUNCTION vdb.chain_row();

-- Append-only: the ledger records history, so history cannot be rewritten.
CREATE OR REPLACE FUNCTION vdb.deny_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'vdb.schema_ledger is append-only — its history cannot be modified';
END;
$$;
CREATE OR REPLACE TRIGGER vdb_append_only BEFORE UPDATE OR DELETE ON vdb.schema_ledger
  FOR EACH ROW EXECUTE FUNCTION vdb.deny_change();
CREATE OR REPLACE TRIGGER vdb_no_truncate BEFORE TRUNCATE ON vdb.schema_ledger
  FOR EACH STATEMENT EXECUTE FUNCTION vdb.deny_change();
REVOKE UPDATE, DELETE, TRUNCATE ON vdb.schema_ledger FROM PUBLIC;

CREATE EVENT TRIGGER vdb_guard_start ON ddl_command_start
  EXECUTE FUNCTION vdb.guard_ddl_start();
CREATE EVENT TRIGGER vdb_log_end ON ddl_command_end
  EXECUTE FUNCTION vdb.log_ddl_end();
CREATE EVENT TRIGGER vdb_log_drop ON sql_drop
  EXECUTE FUNCTION vdb.log_ddl_drop();

-- ── Least-privilege client role ─────────────────────────────────────────────
-- The gateway logs clients in as this NON-superuser role, so a client session is
-- subject to RLS and GRANTs and cannot bypass the append-only ledger — only a
-- superuser can disable a trigger or flip session_replication_role. Roles are
-- cluster-global and copied with a branch's clone; the engine sets its password.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'vdbclient') THEN
    CREATE ROLE vdbclient LOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB NOBYPASSRLS;
  END IF;
END $$;

-- Full access to application data (it owns what it creates; these cover what the
-- superuser created, e.g. imported tables, now and in future).
GRANT USAGE, CREATE ON SCHEMA public TO vdbclient;
GRANT ALL ON ALL TABLES IN SCHEMA public TO vdbclient;
GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO vdbclient;
ALTER DEFAULT PRIVILEGES FOR ROLE vectoradb IN SCHEMA public GRANT ALL ON TABLES TO vdbclient;
ALTER DEFAULT PRIVILEGES FOR ROLE vectoradb IN SCHEMA public GRANT ALL ON SEQUENCES TO vdbclient;

-- The ledger: read only. History is written by the SECURITY DEFINER triggers,
-- not by the client, so a client can read its history and run `verify` but has no
-- INSERT/UPDATE/DELETE on the table at all — it cannot forge or rewrite entries.
GRANT USAGE ON SCHEMA vdb TO vdbclient;
GRANT SELECT ON vdb.schema_ledger, vdb.policy TO vdbclient;
GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA vdb TO vdbclient;
