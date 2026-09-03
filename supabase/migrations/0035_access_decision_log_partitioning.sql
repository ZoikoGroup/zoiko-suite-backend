-- 0035_access_decision_log_partitioning.sql
-- authorization-svc → schema `authorization_svc`. Rebuilds one table.
--
-- The change authorization-svc's own 000009_partition_access_decision_log
-- makes, in the form this project applies it. Same end state; the policy is
-- this project's (app.current_tenant_id()) rather than the compose one.
--
-- ── WHY ─────────────────────────────────────────────────────────────────────
--
-- access_decision_log takes ONE ROW PER AUTHORIZATION EVALUATION,
-- platform-wide, and 000001 wrote it as append-only by design: "No UPDATE or
-- DELETE statement should ever target this." Both statements are true and
-- together they describe a table that grows without bound with no sanctioned
-- way to ever shrink. On a managed project that is not only disk — it is the
-- insert latency on the platform's hottest write, paid on every request
-- forever, against indexes that grow with it.
--
-- DETACH PARTITION removes a month from the live table without issuing a
-- single DELETE, so append-only survives: nothing here ever deletes a
-- decision, it only stops carrying old ones in the hot table. A DELETE-based
-- retention job would rewrite the very evidence the critical constraint ("no
-- material action executes without an authorization decision artifact") exists
-- to preserve.
--
-- ── THE DEFAULT PARTITION IS NOT OPTIONAL ───────────────────────────────────
--
-- A partitioned table with no partition covering an inserted row REJECTS the
-- insert. Here that is not a data error: RecordAccessDecision is the last step
-- before /v1/authorize answers, so a failed insert is a 503. The month after
-- the last one anybody created would take authorization offline platform-wide,
-- at midnight, with an error about a partition range.
--
-- access_decision_log_default catches every such row.
--
-- ── THIS ONE REBUILDS A TABLE, SO READ THE GUARDS ───────────────────────────
--
-- Unlike every other migration in this directory this is not additive: it
-- creates a partitioned table, copies every row across, compares the counts,
-- and only then drops the original. If the counts disagree it raises and the
-- whole thing rolls back with the original untouched.
--
-- It also detects an already-partitioned table and returns, so a project where
-- deployments/supabase has already applied the service's 000009 sees a no-op
-- rather than an error.

DO $guard$
DECLARE
    is_partitioned bool;
    before_count   bigint;
    after_count    bigint;
    m              date;
    i              int;
BEGIN

IF NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'authorization_svc') THEN
    RAISE NOTICE 'schema authorization_svc absent; skipping 0035 — re-run it after deployments/supabase has created the schema';
    RETURN;
END IF;

SELECT c.relkind = 'p' INTO is_partitioned
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'authorization_svc' AND c.relname = 'access_decision_log';

IF is_partitioned IS NULL THEN
    RAISE NOTICE 'authorization_svc.access_decision_log absent; skipping 0035';
    RETURN;
END IF;

IF is_partitioned THEN
    RAISE NOTICE 'authorization_svc.access_decision_log is already partitioned; 0035 is a no-op';
    RETURN;
END IF;

-- ── the partition helper, kept permanently ──────────────────────────────────
--
-- The retention job needs it every month, and an operator pre-creating next
-- quarter's partitions should not hand-write the bound arithmetic. Idempotent.
--
-- SET search_path = '' and fully-qualified names, matching this project's
-- other functions: a SECURITY INVOKER function whose search_path is the
-- caller's would resolve `access_decision_log` to whatever schema the caller
-- happens to be in.
EXECUTE $stmt$
CREATE OR REPLACE FUNCTION authorization_svc.create_access_decision_log_partition(month_start DATE)
RETURNS TEXT
LANGUAGE plpgsql
SECURITY INVOKER
SET search_path = ''
AS $fn$
DECLARE
    from_ts   DATE := date_trunc('month', month_start)::date;
    to_ts     DATE := (date_trunc('month', month_start) + INTERVAL '1 month')::date;
    part_name TEXT := 'access_decision_log_' || to_char(from_ts, 'YYYY_MM');
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'authorization_svc' AND c.relname = part_name
    ) THEN
        RETURN part_name;
    END IF;

    EXECUTE format(
        'CREATE TABLE authorization_svc.%I PARTITION OF authorization_svc.access_decision_log FOR VALUES FROM (%L) TO (%L)',
        part_name, from_ts, to_ts);

    -- The parent's policy covers everything routed THROUGH the parent, which
    -- is all of the service's traffic. Enabled on the partition as well so a
    -- query naming the partition directly -- an operator, a reporting tool,
    -- PostgREST -- is bound by the same tenant predicate. A partition is an
    -- ordinary table and inherits no policy of its own; without this, each new
    -- month would be an unprotected copy of a protected table, reachable
    -- through PostgREST on this project.
    EXECUTE format('ALTER TABLE authorization_svc.%I ENABLE ROW LEVEL SECURITY', part_name);
    EXECUTE format('ALTER TABLE authorization_svc.%I FORCE  ROW LEVEL SECURITY', part_name);
    EXECUTE format(
        'CREATE POLICY tenant_isolation_policy ON authorization_svc.%I FOR ALL'
        ' USING (tenant_id IS NULL OR tenant_id::text = app.current_tenant_id())'
        ' WITH CHECK ((tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())'
        '             OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL))',
        part_name);

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON authorization_svc.%I TO app_authorization', part_name);
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_backend') THEN
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON authorization_svc.%I TO zoiko_backend', part_name);
    END IF;

    RETURN part_name;
END
$fn$
$stmt$;

-- ── the swap ────────────────────────────────────────────────────────────────

EXECUTE $stmt$ALTER TABLE authorization_svc.access_decision_log RENAME TO access_decision_log_pre_partition$stmt$;

-- Postgres requires a partitioned table's unique constraints to include every
-- partition-key column, so the key becomes (access_decision_id, decided_at).
-- The rationale read (FindAccessDecisionByID) filters on
-- (access_decision_id, tenant_id) with no date, so it is served by an explicit
-- single-column index rather than by the key.
EXECUTE $stmt$
CREATE TABLE authorization_svc.access_decision_log (
    access_decision_id       UUID         NOT NULL DEFAULT gen_random_uuid(),
    principal_id             TEXT         NOT NULL,
    legal_entity_id          UUID         NOT NULL,
    action_type              VARCHAR(128) NOT NULL,
    decision_outcome         VARCHAR(16)  NOT NULL,
    decision_basis           TEXT         NOT NULL,
    tenant_id                UUID,
    correlation_id           TEXT,
    decided_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (access_decision_id, decided_at)
) PARTITION BY RANGE (decided_at)
$stmt$;

-- The catch-all, created FIRST so there is never an instant where an insert
-- could find no home.
EXECUTE $stmt$CREATE TABLE authorization_svc.access_decision_log_default PARTITION OF authorization_svc.access_decision_log DEFAULT$stmt$;
EXECUTE $stmt$ALTER TABLE authorization_svc.access_decision_log_default ENABLE ROW LEVEL SECURITY$stmt$;
EXECUTE $stmt$ALTER TABLE authorization_svc.access_decision_log_default FORCE  ROW LEVEL SECURITY$stmt$;
EXECUTE $stmt$
CREATE POLICY tenant_isolation_policy ON authorization_svc.access_decision_log_default
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = app.current_tenant_id())
    WITH CHECK ((tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())
                OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL))
$stmt$;

-- Every month present in the existing data, so the copy lands in real
-- partitions rather than piling into the default one...
FOR m IN
    SELECT DISTINCT date_trunc('month', decided_at)::date
      FROM authorization_svc.access_decision_log_pre_partition
     ORDER BY 1
LOOP
    PERFORM authorization_svc.create_access_decision_log_partition(m);
END LOOP;

-- ...and the current month plus the next three, so a project that never
-- schedules the retention job has a runway rather than an outage.
FOR i IN 0..3 LOOP
    PERFORM authorization_svc.create_access_decision_log_partition((CURRENT_DATE + (i || ' month')::interval)::date);
END LOOP;

EXECUTE $stmt$
INSERT INTO authorization_svc.access_decision_log (
    access_decision_id, principal_id, legal_entity_id, action_type,
    decision_outcome, decision_basis, tenant_id, correlation_id, decided_at)
SELECT
    access_decision_id, principal_id, legal_entity_id, action_type,
    decision_outcome, decision_basis, tenant_id, correlation_id, decided_at
FROM authorization_svc.access_decision_log_pre_partition
$stmt$;

SELECT count(*) INTO before_count FROM authorization_svc.access_decision_log_pre_partition;
SELECT count(*) INTO after_count  FROM authorization_svc.access_decision_log;
IF before_count <> after_count THEN
    RAISE EXCEPTION
        'access_decision_log partition copy lost rows: % before, % after. Rolled back; the original table is untouched.',
        before_count, after_count;
END IF;

EXECUTE $stmt$DROP TABLE authorization_svc.access_decision_log_pre_partition$stmt$;

-- ── indexes on the parent, which propagate to every partition ───────────────

EXECUTE $stmt$CREATE INDEX idx_access_decision_log_principal ON authorization_svc.access_decision_log (principal_id, decided_at DESC)$stmt$;
EXECUTE $stmt$CREATE INDEX idx_access_decision_log_entity    ON authorization_svc.access_decision_log (legal_entity_id, decided_at DESC)$stmt$;
EXECUTE $stmt$CREATE INDEX idx_access_decision_log_tenant    ON authorization_svc.access_decision_log (tenant_id, decided_at DESC)$stmt$;
EXECUTE $stmt$CREATE INDEX idx_access_decision_log_id        ON authorization_svc.access_decision_log (access_decision_id)$stmt$;

-- ── the parent's policy, restored to 0028's form ────────────────────────────
--
-- Identical to what 0028 installed, and NULL is admitted on the USING side for
-- the same concrete reason: RecordAccessDecision uses RETURNING, Postgres
-- applies the SELECT side of a FOR ALL policy to an INSERT ... RETURNING, and a
-- USING clause excluding NULL would make recording a tenantless decision fail
-- outright — a 503 on every request from a caller that sends no X-Tenant-Id,
-- which is most of them. Tenant isolation on the READ path is the store's
-- explicit `tenant_id = $2` predicate; this policy is the backstop.

EXECUTE $stmt$ALTER TABLE authorization_svc.access_decision_log ENABLE ROW LEVEL SECURITY$stmt$;
EXECUTE $stmt$ALTER TABLE authorization_svc.access_decision_log FORCE  ROW LEVEL SECURITY$stmt$;
EXECUTE $stmt$
CREATE POLICY tenant_isolation_policy ON authorization_svc.access_decision_log
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id::text = app.current_tenant_id()
    )
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())
        OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL)
    )
$stmt$;

IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
    EXECUTE $stmt$GRANT SELECT, INSERT, UPDATE, DELETE ON authorization_svc.access_decision_log TO app_authorization$stmt$;
END IF;
IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_backend') THEN
    EXECUTE $stmt$GRANT SELECT, INSERT, UPDATE, DELETE ON authorization_svc.access_decision_log TO zoiko_backend$stmt$;
END IF;

-- ── retention ───────────────────────────────────────────────────────────────
--
-- DETACHes every whole month that ends on or before cutoff and reports what it
-- detached. It does NOT drop anything: a detached partition is an ordinary
-- table in the same schema, which is the point — the operator archives it and
-- drops it as a separate, deliberate act.
--
-- The default partition is never detached, whatever the cutoff: it holds rows
-- whose dates nobody anticipated, so its contents are not "everything before
-- cutoff", and detaching it would remove the safety net the parent depends on.
EXECUTE $stmt$
CREATE OR REPLACE FUNCTION authorization_svc.detach_access_decision_log_partitions_before(cutoff DATE)
RETURNS TABLE (partition_name TEXT, row_count BIGINT)
LANGUAGE plpgsql
SECURITY INVOKER
SET search_path = ''
AS $fn$
DECLARE
    part RECORD;
    n    BIGINT;
BEGIN
    FOR part IN
        SELECT c.relname AS relname,
               pg_get_expr(c.relpartbound, c.oid) AS bound
          FROM pg_class c
          JOIN pg_inherits i ON i.inhrelid = c.oid
          JOIN pg_class p ON p.oid = i.inhparent
          JOIN pg_namespace n ON n.oid = p.relnamespace
         WHERE n.nspname = 'authorization_svc'
           AND p.relname = 'access_decision_log'
           AND c.relname <> 'access_decision_log_default'
         ORDER BY c.relname
    LOOP
        CONTINUE WHEN part.bound IS NULL;
        CONTINUE WHEN part.bound LIKE '%DEFAULT%';
        CONTINUE WHEN substring(part.bound from 'TO \(''([0-9-]{10})')::date > cutoff;

        EXECUTE format('SELECT count(*) FROM authorization_svc.%I', part.relname) INTO n;
        EXECUTE format('ALTER TABLE authorization_svc.access_decision_log DETACH PARTITION authorization_svc.%I', part.relname);

        partition_name := part.relname;
        row_count := n;
        RETURN NEXT;
    END LOOP;
END
$fn$
$stmt$;

EXECUTE $stmt$
CREATE OR REPLACE VIEW authorization_svc.access_decision_log_retention_status AS
SELECT
    c.relname                                     AS partition_name,
    pg_size_pretty(pg_total_relation_size(c.oid)) AS total_size,
    (c.relname = 'access_decision_log_default')   AS is_default,
    pg_get_expr(c.relpartbound, c.oid)            AS partition_bound
  FROM pg_class c
  JOIN pg_inherits i ON i.inhrelid = c.oid
  JOIN pg_class p ON p.oid = i.inhparent
  JOIN pg_namespace n ON n.oid = p.relnamespace
 WHERE n.nspname = 'authorization_svc'
   AND p.relname = 'access_decision_log'
 ORDER BY c.relname
$stmt$;

-- ── Verification ────────────────────────────────────────────────────────────

DECLARE unprotected int;
BEGIN
    -- Every partition, and the parent, must carry forced row security. A
    -- partition without it is an unprotected copy of a protected table, and on
    -- this project PostgREST can reach it.
    SELECT count(*) INTO unprotected
      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'authorization_svc'
       AND c.relkind IN ('r', 'p')
       AND NOT (c.relrowsecurity AND c.relforcerowsecurity);
    IF unprotected > 0 THEN
        RAISE EXCEPTION
            '% authorization_svc tables or partitions lack forced row security after 0035', unprotected;
    END IF;

    RAISE NOTICE '0035 applied: % decision artifacts now live in monthly partitions, with a default partition and a DETACH-based retention function.',
        after_count;
END;

END
$guard$;
