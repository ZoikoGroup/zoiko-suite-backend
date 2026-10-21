-- ZS-SVC-L-001 WFP-02 — Time & Leave Financial Import.
--
-- Frozen, hashed snapshots of APPROVED leave for a payroll period, with control
-- totals. This service does not decide attendance truth or leave approval — it
-- imports what the approval workflow has already approved, validates it against
-- the worker's financial context, and freezes it so payroll calculates from a
-- stable population. Late changes arrive as a superseding batch, never as an
-- edit to a frozen one.
--
-- Scope (decision 2026-10-14): leave only. There is no worked-time source yet;
-- source_system_id / source_line_id keep the model source-agnostic.

CREATE TABLE IF NOT EXISTS wfp_time_leave_batches (
    batch_id                UUID PRIMARY KEY,
    tenant_id               TEXT NOT NULL,
    legal_entity_id         TEXT NOT NULL,
    payroll_period_id       TEXT NOT NULL,
    period_start            DATE NOT NULL,
    period_end              DATE NOT NULL,
    source_system_id        TEXT NOT NULL,
    batch_version           INTEGER NOT NULL DEFAULT 1,
    on_invalid              TEXT NOT NULL,
    state                   TEXT NOT NULL,
    rejection_reason        TEXT,
    line_count              INTEGER NOT NULL DEFAULT 0,
    accepted_line_count     INTEGER NOT NULL DEFAULT 0,
    quarantined_line_count  INTEGER NOT NULL DEFAULT 0,
    -- Exact decimal hours; never a float.
    total_units             NUMERIC(14, 2) NOT NULL DEFAULT 0,
    paid_units              NUMERIC(14, 2) NOT NULL DEFAULT 0,
    unpaid_units            NUMERIC(14, 2) NOT NULL DEFAULT 0,
    unit_of_measure         TEXT NOT NULL DEFAULT 'HOURS',
    source_hash             TEXT,
    normalization_version   TEXT NOT NULL,
    supersedes_batch_id     UUID REFERENCES wfp_time_leave_batches (batch_id),
    version                 INTEGER NOT NULL DEFAULT 1,
    frozen_at               TIMESTAMPTZ,
    frozen_by               TEXT,
    correlation_id          TEXT NOT NULL,
    created_by              TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_tl_batch_state_check CHECK (state IN ('RECEIVED', 'ACCEPTED', 'FROZEN', 'SUPERSEDED', 'REJECTED')),
    CONSTRAINT wfp_tl_batch_policy_check CHECK (on_invalid IN ('QUARANTINE_LINES', 'REJECT_BATCH')),
    CONSTRAINT wfp_tl_batch_period_check CHECK (period_end >= period_start),
    CONSTRAINT wfp_tl_batch_counts_check CHECK (accepted_line_count + quarantined_line_count = line_count),
    CONSTRAINT wfp_tl_batch_totals_check CHECK (total_units = paid_units + unpaid_units),
    CONSTRAINT wfp_tl_batch_frozen_check CHECK ((state = 'FROZEN') = (frozen_at IS NOT NULL) OR state = 'SUPERSEDED'),
    CONSTRAINT wfp_tl_batch_unique_version UNIQUE (tenant_id, legal_entity_id, payroll_period_id, source_system_id, batch_version)
);

-- One live batch per entity + period + source: a second import must go through
-- SupersedeBatch, so two competing populations can never both feed payroll.
CREATE UNIQUE INDEX IF NOT EXISTS uq_wfp_tl_batch_live
    ON wfp_time_leave_batches (tenant_id, legal_entity_id, payroll_period_id, source_system_id)
    WHERE state IN ('RECEIVED', 'ACCEPTED', 'FROZEN');

CREATE TABLE IF NOT EXISTS wfp_time_leave_lines (
    line_id            UUID PRIMARY KEY,
    tenant_id          TEXT NOT NULL,
    batch_id           UUID NOT NULL REFERENCES wfp_time_leave_batches (batch_id),
    source_line_id     TEXT NOT NULL,
    worker_id          UUID NOT NULL,
    leave_type_id      UUID NOT NULL,
    leave_type_code    TEXT NOT NULL,
    is_paid            BOOLEAN NOT NULL,
    start_date         DATE NOT NULL,
    end_date           DATE NOT NULL,
    units              NUMERIC(10, 2) NOT NULL,
    unit_of_measure    TEXT NOT NULL DEFAULT 'HOURS',
    approved_by        TEXT,
    approved_at        TIMESTAMPTZ,
    state              TEXT NOT NULL,
    quarantine_reason  TEXT,
    line_hash          TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_tl_line_state_check CHECK (state IN ('ACCEPTED', 'QUARANTINED')),
    CONSTRAINT wfp_tl_line_reason_check CHECK ((state = 'QUARANTINED') = (quarantine_reason IS NOT NULL)),
    CONSTRAINT wfp_tl_line_units_check CHECK (units >= 0),
    -- A source line is one financial unit, however many times it is delivered.
    CONSTRAINT wfp_tl_line_unique_source UNIQUE (batch_id, source_line_id)
);
CREATE INDEX IF NOT EXISTS idx_wfp_tl_line_batch ON wfp_time_leave_lines (tenant_id, batch_id, state);
CREATE INDEX IF NOT EXISTS idx_wfp_tl_line_worker ON wfp_time_leave_lines (tenant_id, batch_id, worker_id);

-- ── Guards ───────────────────────────────────────────────────────────────────
-- A FROZEN batch is immutable; it can only be superseded. A batch's identity
-- never changes. SUPERSEDED and REJECTED are terminal.
CREATE OR REPLACE FUNCTION wfp_tl_batch_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'time/leave batches are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.payroll_period_id IS DISTINCT FROM OLD.payroll_period_id
       OR NEW.period_start IS DISTINCT FROM OLD.period_start
       OR NEW.period_end IS DISTINCT FROM OLD.period_end
       OR NEW.source_system_id IS DISTINCT FROM OLD.source_system_id
       OR NEW.batch_version IS DISTINCT FROM OLD.batch_version
       OR NEW.supersedes_batch_id IS DISTINCT FROM OLD.supersedes_batch_id THEN
        RAISE EXCEPTION 'batch identity is immutable; a changed population is a new batch version'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('SUPERSEDED', 'REJECTED') AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION '% is terminal', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'FROZEN' THEN
        IF NEW.state NOT IN ('FROZEN', 'SUPERSEDED') THEN
            RAISE EXCEPTION 'a FROZEN batch can only be superseded' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.line_count IS DISTINCT FROM OLD.line_count
           OR NEW.accepted_line_count IS DISTINCT FROM OLD.accepted_line_count
           OR NEW.quarantined_line_count IS DISTINCT FROM OLD.quarantined_line_count
           OR NEW.total_units IS DISTINCT FROM OLD.total_units
           OR NEW.paid_units IS DISTINCT FROM OLD.paid_units
           OR NEW.unpaid_units IS DISTINCT FROM OLD.unpaid_units
           OR NEW.source_hash IS DISTINCT FROM OLD.source_hash THEN
            RAISE EXCEPTION 'a FROZEN batch population and control totals are immutable'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wfp_tl_batch_guard_trg ON wfp_time_leave_batches;
CREATE TRIGGER wfp_tl_batch_guard_trg
    BEFORE UPDATE OR DELETE ON wfp_time_leave_batches
    FOR EACH ROW EXECUTE FUNCTION wfp_tl_batch_guard();

-- Lines are only written while their batch is RECEIVED (the import transaction),
-- and only ever quarantined (never edited) while it is ACCEPTED.
CREATE OR REPLACE FUNCTION wfp_tl_line_guard() RETURNS trigger AS $$
DECLARE b_state TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'time/leave lines are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT state INTO b_state FROM wfp_time_leave_batches WHERE batch_id = NEW.batch_id;
    IF TG_OP = 'INSERT' THEN
        IF b_state IS DISTINCT FROM 'RECEIVED' THEN
            RAISE EXCEPTION 'lines can only be added while the batch is RECEIVED' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    -- UPDATE
    IF b_state IS DISTINCT FROM 'ACCEPTED' THEN
        RAISE EXCEPTION 'lines of a % batch are immutable', b_state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.batch_id IS DISTINCT FROM OLD.batch_id
       OR NEW.source_line_id IS DISTINCT FROM OLD.source_line_id OR NEW.worker_id IS DISTINCT FROM OLD.worker_id
       OR NEW.leave_type_id IS DISTINCT FROM OLD.leave_type_id OR NEW.units IS DISTINCT FROM OLD.units
       OR NEW.start_date IS DISTINCT FROM OLD.start_date OR NEW.end_date IS DISTINCT FROM OLD.end_date
       OR NEW.is_paid IS DISTINCT FROM OLD.is_paid OR NEW.line_hash IS DISTINCT FROM OLD.line_hash THEN
        RAISE EXCEPTION 'line content is immutable; quarantine it or supersede the batch' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'QUARANTINED' AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION 'a quarantined line stays quarantined' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wfp_tl_line_guard_trg ON wfp_time_leave_lines;
CREATE TRIGGER wfp_tl_line_guard_trg
    BEFORE INSERT OR UPDATE OR DELETE ON wfp_time_leave_lines
    FOR EACH ROW EXECUTE FUNCTION wfp_tl_line_guard();

-- ── Outbox (same shape as the other WFP services) ────────────────────────────
CREATE TABLE IF NOT EXISTS wfp_outbox (
    outbox_id     BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    object_id     TEXT NOT NULL,
    event_type    TEXT NOT NULL,
    payload       JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at  TIMESTAMPTZ,
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT
);
CREATE INDEX IF NOT EXISTS idx_wfp_outbox_unpublished
    ON wfp_outbox (created_at, outbox_id) WHERE published_at IS NULL;

-- ── Row-level security ───────────────────────────────────────────────────────
ALTER TABLE wfp_time_leave_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_time_leave_batches FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_time_leave_batches FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_time_leave_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_time_leave_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_time_leave_lines FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_tenant_isolation ON wfp_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
