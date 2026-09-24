-- Migration: 000009_partition_access_decision_log.up.sql
--
-- Converts access_decision_log to a monthly RANGE-partitioned table and gives
-- it a retention mechanism it has never had.
--
-- ── WHY ─────────────────────────────────────────────────────────────────────
--
-- This table takes ONE ROW PER AUTHORIZATION EVALUATION, platform-wide.
-- /v1/authorize is called on nearly every mutating request across ~60
-- services, and 000001 wrote the table as append-only by design: "No
-- UPDATE or DELETE statement should ever target this." Both statements are
-- correct and together they describe a table that grows without bound and has
-- no sanctioned way to ever shrink. The cost is not only disk — it is the
-- insert latency on the platform's hottest write, paid on every request
-- forever, against three indexes that grow with it.
--
-- ── WHY PARTITIONING RATHER THAN A DELETE JOB ───────────────────────────────
--
-- A retention job that DELETEs is the obvious answer and it is the wrong one
-- here. It contradicts 000001's append-only guarantee directly, it rewrites
-- the very evidence the critical constraint ("no material action executes
-- without an authorization decision artifact") exists to preserve, and it does
-- the work row by row against those same three indexes.
--
-- DETACH PARTITION removes a month from the live table without issuing a
-- single DELETE. The rows still exist, in a plain table, ready to be copied to
-- cold storage and only then dropped — by an operator, deliberately, as a
-- separate act. Append-only survives: nothing in this schema ever deletes a
-- decision, it only stops carrying old ones in the hot table.
--
-- ── THE DEFAULT PARTITION IS NOT OPTIONAL ───────────────────────────────────
--
-- A partitioned table with no partition covering an inserted row REJECTS the
-- insert. On this table that is not a data error — RecordAccessDecision is the
-- last step before /v1/authorize answers, and a failed insert there is a 503.
-- So the month after the last one anybody remembered to create would take
-- authorization offline platform-wide, at midnight, with an error message
-- about a partition range.
--
-- access_decision_log_default catches every such row. The service keeps
-- answering; the only symptom is rows accumulating in the default partition,
-- which access_decision_log_retention_status reports rather than silently
-- swallowing.
--
-- ── PRIMARY KEY ─────────────────────────────────────────────────────────────
--
-- Postgres requires a partitioned table's unique constraints to include every
-- partition-key column, so the key becomes (access_decision_id, decided_at).
-- access_decision_id is still globally unique in practice (a v4 UUID from
-- gen_random_uuid), and the read path — FindAccessDecisionByID, which filters
-- on (access_decision_id, tenant_id) with no date — is served by an explicit
-- index on access_decision_id rather than by the key. That read now touches
-- every partition, which is the honest cost of not making callers quote a date
-- they were never given; it is a point lookup on a small index per partition,
-- and the partition count is bounded by the retention window.
--
-- ── ATOMIC, AND IT WOULD RATHER FAIL ────────────────────────────────────────
--
-- Everything below is one transaction, and the row counts are compared before
-- the old table is dropped. A copy that loses a decision artifact aborts the
-- migration instead of completing it.

BEGIN;

-- ── the partition-creation helper, kept permanently ─────────────────────────
--
-- Permanent because the retention job needs it every month, and because an
-- operator pre-creating next quarter's partitions should not have to hand-write
-- the bound arithmetic. Idempotent: creating a partition that exists is a
-- no-op, not an error.
CREATE OR REPLACE FUNCTION create_access_decision_log_partition(month_start DATE)
RETURNS TEXT
LANGUAGE plpgsql
AS $fn$
DECLARE
    from_ts   DATE := date_trunc('month', month_start)::date;
    to_ts     DATE := (date_trunc('month', month_start) + INTERVAL '1 month')::date;
    part_name TEXT := 'access_decision_log_' || to_char(from_ts, 'YYYY_MM');
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = part_name) THEN
        RETURN part_name;
    END IF;

    EXECUTE format(
        'CREATE TABLE %I PARTITION OF access_decision_log FOR VALUES FROM (%L) TO (%L)',
        part_name, from_ts, to_ts);

    -- RLS on the parent covers every read and write that goes THROUGH the
    -- parent, which is all of them from this service. It is enabled on the
    -- partition as well so a query naming the partition directly — an
    -- operator, a reporting tool, a future service reaching past the parent —
    -- is bound by the same tenant predicate. A partition is an ordinary table
    -- and inherits no policy of its own; without this, each new month would be
    -- an unprotected copy of a protected table.
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', part_name);
    EXECUTE format('ALTER TABLE %I FORCE  ROW LEVEL SECURITY', part_name);
    EXECUTE format(
        'CREATE POLICY tenant_isolation_policy ON %I FOR ALL'
        ' USING      (tenant_id IS NULL OR tenant_id = NULLIF(current_setting(''app.tenant_id'', true), '''')::uuid)'
        ' WITH CHECK (tenant_id IS NULL OR tenant_id = NULLIF(current_setting(''app.tenant_id'', true), '''')::uuid)',
        part_name);

    RETURN part_name;
END
$fn$;

COMMENT ON FUNCTION create_access_decision_log_partition(DATE) IS
    'Creates (idempotently) the monthly access_decision_log partition containing month_start, with the same row-level security as the parent. Returns the partition name.';

-- ── the swap ────────────────────────────────────────────────────────────────

ALTER TABLE access_decision_log RENAME TO access_decision_log_pre_partition;

-- Column list copied from 000001 + 000005's tenant_id, deliberately spelled
-- out rather than derived: this is the shape the store's accessDecisionColumns
-- constant names, and a LIKE clause would hide a drift between them.
CREATE TABLE access_decision_log (
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
) PARTITION BY RANGE (decided_at);

COMMENT ON COLUMN access_decision_log.tenant_id IS
    'Verified tenant scope the decision was evaluated in. NULL when the caller supplied none (pre-000005 rows, and callers that send no tenant); NULL rows are not readable via GET /v1/access-decisions/{id}.';

-- The catch-all, created FIRST so there is never an instant in this
-- transaction where an insert could find no home.
CREATE TABLE access_decision_log_default PARTITION OF access_decision_log DEFAULT;
ALTER TABLE access_decision_log_default ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_decision_log_default FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON access_decision_log_default
    FOR ALL
    USING      (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Every month present in the existing data, so the copy below lands in real
-- partitions rather than piling into the default one...
DO $$
DECLARE m DATE;
BEGIN
    FOR m IN
        SELECT DISTINCT date_trunc('month', decided_at)::date
        FROM access_decision_log_pre_partition
        ORDER BY 1
    LOOP
        PERFORM create_access_decision_log_partition(m);
    END LOOP;
END
$$;

-- ...and the current month plus the next three, so a deployment that never
-- schedules the retention job still has a runway rather than an outage.
DO $$
DECLARE i INT;
BEGIN
    FOR i IN 0..3 LOOP
        PERFORM create_access_decision_log_partition((CURRENT_DATE + (i || ' month')::interval)::date);
    END LOOP;
END
$$;

INSERT INTO access_decision_log (
    access_decision_id, principal_id, legal_entity_id, action_type,
    decision_outcome, decision_basis, tenant_id, correlation_id, decided_at)
SELECT
    access_decision_id, principal_id, legal_entity_id, action_type,
    decision_outcome, decision_basis, tenant_id, correlation_id, decided_at
FROM access_decision_log_pre_partition;

DO $$
DECLARE before_count BIGINT; after_count BIGINT;
BEGIN
    SELECT count(*) INTO before_count FROM access_decision_log_pre_partition;
    SELECT count(*) INTO after_count  FROM access_decision_log;
    IF before_count <> after_count THEN
        RAISE EXCEPTION
            'access_decision_log partition copy lost rows: % before, % after. Migration aborted; the original table is untouched.',
            before_count, after_count;
    END IF;
    RAISE NOTICE 'access_decision_log: % decision artifacts copied into partitions.', after_count;
END
$$;

DROP TABLE access_decision_log_pre_partition;

-- ── indexes ─────────────────────────────────────────────────────────────────
--
-- Created on the parent, which propagates them to every existing partition and
-- to every partition created later — including by
-- create_access_decision_log_partition, so a new month is never briefly
-- unindexed. Same three as 000001 + 000005.

CREATE INDEX idx_access_decision_log_principal ON access_decision_log (principal_id, decided_at DESC);
CREATE INDEX idx_access_decision_log_entity    ON access_decision_log (legal_entity_id, decided_at DESC);
CREATE INDEX idx_access_decision_log_tenant    ON access_decision_log (tenant_id, decided_at DESC);

-- The rationale-retrieval lookup. It was the primary key's job before this
-- migration; the key now leads with (access_decision_id, decided_at) and
-- FindAccessDecisionByID has no date to give it, so the single-column index is
-- what keeps that read a point lookup.
CREATE INDEX idx_access_decision_log_id ON access_decision_log (access_decision_id);

-- ── row level security on the parent ────────────────────────────────────────
--
-- Identical to 000005's policy, and identical for the same reason: the insert
-- in RecordAccessDecision uses RETURNING, Postgres applies the SELECT side of a
-- FOR ALL policy to an INSERT ... RETURNING, and a USING clause excluding NULL
-- would make recording a tenantless decision fail outright. Tenant isolation on
-- the READ path is the store's explicit `tenant_id = $2` predicate; this policy
-- is the backstop.

ALTER TABLE access_decision_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_decision_log FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON access_decision_log
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- ── retention ───────────────────────────────────────────────────────────────
--
-- DETACHes every whole month that ends on or before cutoff, and returns what it
-- detached. It does NOT drop anything: a detached partition is a plain table
-- sitting in the same database, which is the point — the operator archives it
-- and drops it as a separate, deliberate act.
--
-- CONCURRENTLY is deliberately NOT used. It cannot run inside a transaction
-- block, and this function is called by a scheduled job that should either
-- detach a whole set of months or none. The lock taken is on the partition
-- being detached, not on the parent's inserts into the current month, so the
-- hot path is unaffected.
--
-- The default partition is never detached, whatever the cutoff: it holds rows
-- whose dates nobody anticipated, so its contents are not "everything before
-- cutoff", and detaching it would remove the safety net the parent depends on.
-- Its contents are reported instead — see access_decision_log_retention_status.
CREATE OR REPLACE FUNCTION detach_access_decision_log_partitions_before(cutoff DATE)
RETURNS TABLE (partition_name TEXT, row_count BIGINT)
LANGUAGE plpgsql
AS $fn$
DECLARE
    part RECORD;
    n    BIGINT;
BEGIN
    FOR part IN
        SELECT c.relname AS relname,
               -- pg_get_expr renders the bound as
               --   FOR VALUES FROM ('2026-01-01 ...') TO ('2026-02-01 ...')
               -- and the upper bound is what decides whether the whole month is
               -- older than the cutoff.
               pg_get_expr(c.relpartbound, c.oid) AS bound
          FROM pg_class c
          JOIN pg_inherits i ON i.inhrelid = c.oid
          JOIN pg_class p ON p.oid = i.inhparent
         WHERE p.relname = 'access_decision_log'
           AND c.relname <> 'access_decision_log_default'
         ORDER BY c.relname
    LOOP
        CONTINUE WHEN part.bound IS NULL;
        CONTINUE WHEN part.bound LIKE '%DEFAULT%';
        CONTINUE WHEN substring(part.bound from 'TO \(''([0-9-]{10})')::date > cutoff;

        EXECUTE format('SELECT count(*) FROM %I', part.relname) INTO n;
        EXECUTE format('ALTER TABLE access_decision_log DETACH PARTITION %I', part.relname);

        partition_name := part.relname;
        row_count := n;
        RETURN NEXT;
    END LOOP;
END
$fn$;

COMMENT ON FUNCTION detach_access_decision_log_partitions_before(DATE) IS
    'Detaches every access_decision_log monthly partition whose range ends on or before cutoff, returning (partition_name, row_count). Detached partitions remain as ordinary tables: archive, then drop them deliberately. Never detaches access_decision_log_default.';

-- What a scheduled job or an operator reads to know whether the runway is
-- holding: which months exist, how far ahead they reach, and whether anything
-- has landed in the default partition (which means a month was missing when a
-- decision was recorded).
CREATE OR REPLACE VIEW access_decision_log_retention_status AS
SELECT
    c.relname                                     AS partition_name,
    pg_size_pretty(pg_total_relation_size(c.oid)) AS total_size,
    (c.relname = 'access_decision_log_default')   AS is_default,
    pg_get_expr(c.relpartbound, c.oid)            AS partition_bound
  FROM pg_class c
  JOIN pg_inherits i ON i.inhrelid = c.oid
  JOIN pg_class p ON p.oid = i.inhparent
 WHERE p.relname = 'access_decision_log'
 ORDER BY c.relname;

COMMENT ON VIEW access_decision_log_retention_status IS
    'One row per access_decision_log partition with its size and range. A non-empty access_decision_log_default means decisions were recorded for a month with no partition: call create_access_decision_log_partition() for the missing months and move those rows.';

COMMIT;
