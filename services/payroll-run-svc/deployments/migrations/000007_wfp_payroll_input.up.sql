-- ZS-SVC-L-001 WFP-03 — Payroll Input Interface.
--
-- Frozen, versioned, source-linked populations of earning / deduction / benefit
-- inputs for a payroll period, plus the governed code mapping that classifies
-- them. This service does not calculate pay: an input line is a fact pulled from
-- its authoritative source (contract, compensation, benefits) with its
-- provenance, classified by an effective-dated mapping, and frozen so the payroll
-- engine calculates from a stable population.
--
-- Decisions (2026-10-14): tenant-level mapping table here; inputs are PULLED at
-- batch creation; no manual lines yet; CalculateRun is not changed in this wave.

-- ── Input code mappings (WFP-D-03) ───────────────────────────────────────────
-- source code -> payroll code, category and tax treatment. Append-only: a change
-- is a new row with a later effective_from and the next mapping_version, never an
-- edit, so a frozen batch can always say which mapping it used.
CREATE TABLE IF NOT EXISTS wfp_input_code_mappings (
    mapping_id        UUID PRIMARY KEY,
    tenant_id         TEXT NOT NULL,
    source_kind       TEXT NOT NULL,
    source_code       TEXT NOT NULL,
    mapping_version   INTEGER NOT NULL,
    payroll_code      TEXT NOT NULL,
    category          TEXT NOT NULL,
    tax_treatment     TEXT NOT NULL,
    -- false = from effective_from this source code is deliberately unmapped.
    active            BOOLEAN NOT NULL DEFAULT true,
    effective_from    DATE NOT NULL,
    registered_by     TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_map_kind_check CHECK (source_kind IN
        ('BASE_SALARY', 'COMP_COMPONENT', 'COMP_BONUS', 'BENEFIT_EMPLOYEE', 'BENEFIT_EMPLOYER')),
    CONSTRAINT wfp_map_category_check CHECK (category IN ('EARNING', 'DEDUCTION', 'BENEFIT', 'REIMBURSEMENT', 'ADJUSTMENT')),
    CONSTRAINT wfp_map_tax_check CHECK (tax_treatment IN ('TAXABLE', 'NON_TAXABLE', 'PRE_TAX', 'POST_TAX', 'NOT_APPLICABLE')),
    CONSTRAINT wfp_map_version_check CHECK (mapping_version >= 1),
    CONSTRAINT wfp_map_unique_version UNIQUE (tenant_id, source_kind, source_code, mapping_version),
    CONSTRAINT wfp_map_unique_effective UNIQUE (tenant_id, source_kind, source_code, effective_from)
);

-- ── Input batches ────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS wfp_payroll_input_batches (
    batch_id                UUID PRIMARY KEY,
    tenant_id               TEXT NOT NULL,
    legal_entity_id         TEXT NOT NULL,
    payroll_period_id       TEXT NOT NULL,
    period_start            DATE NOT NULL,
    period_end              DATE NOT NULL,
    batch_version           INTEGER NOT NULL DEFAULT 1,
    on_invalid              TEXT NOT NULL,
    state                   TEXT NOT NULL,
    rejection_reason        TEXT,
    worker_count            INTEGER NOT NULL DEFAULT 0,
    line_count              INTEGER NOT NULL DEFAULT 0,
    accepted_line_count     INTEGER NOT NULL DEFAULT 0,
    quarantined_line_count  INTEGER NOT NULL DEFAULT 0,
    source_hash             TEXT,
    normalization_version   TEXT NOT NULL,
    supersedes_batch_id     UUID REFERENCES wfp_payroll_input_batches (batch_id),
    payroll_run_id          UUID,
    version                 INTEGER NOT NULL DEFAULT 1,
    frozen_at               TIMESTAMPTZ,
    frozen_by               TEXT,
    consumed_at             TIMESTAMPTZ,
    correlation_id          TEXT NOT NULL,
    created_by              TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_in_batch_state_check CHECK (state IN
        ('RECEIVED', 'READY', 'FROZEN', 'CONSUMED', 'SUPERSEDED', 'REJECTED', 'CANCELLED')),
    CONSTRAINT wfp_in_batch_policy_check CHECK (on_invalid IN ('QUARANTINE_LINES', 'REJECT_BATCH')),
    CONSTRAINT wfp_in_batch_period_check CHECK (period_end >= period_start),
    CONSTRAINT wfp_in_batch_counts_check CHECK (accepted_line_count + quarantined_line_count = line_count),
    CONSTRAINT wfp_in_batch_consumed_check CHECK ((state = 'CONSUMED') = (payroll_run_id IS NOT NULL)),
    CONSTRAINT wfp_in_batch_unique_version UNIQUE (tenant_id, legal_entity_id, payroll_period_id, batch_version)
);

-- One live population per entity + period: a second pull must go through
-- SupersedeBatch, so two competing input sets can never both feed payroll.
CREATE UNIQUE INDEX IF NOT EXISTS uq_wfp_in_batch_live
    ON wfp_payroll_input_batches (tenant_id, legal_entity_id, payroll_period_id)
    WHERE state IN ('RECEIVED', 'READY', 'FROZEN', 'CONSUMED');
-- A payroll run consumes at most one input batch.
CREATE UNIQUE INDEX IF NOT EXISTS uq_wfp_in_batch_run
    ON wfp_payroll_input_batches (tenant_id, payroll_run_id) WHERE payroll_run_id IS NOT NULL;

-- ── Input lines ──────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS wfp_payroll_input_lines (
    line_id            UUID PRIMARY KEY,
    tenant_id          TEXT NOT NULL,
    batch_id           UUID NOT NULL REFERENCES wfp_payroll_input_batches (batch_id),
    worker_id          UUID NOT NULL,
    source_kind        TEXT NOT NULL,
    source_line_id     TEXT NOT NULL,
    source_ref         TEXT,
    input_code         TEXT NOT NULL,
    payroll_code       TEXT,
    category           TEXT NOT NULL,
    tax_treatment      TEXT,
    mapping_id         UUID REFERENCES wfp_input_code_mappings (mapping_id),
    mapping_version    INTEGER,
    -- Exact decimal; the sign is carried by the category, never by the amount.
    amount             NUMERIC(18, 4) NOT NULL,
    currency           TEXT NOT NULL,
    effective_date     DATE NOT NULL,
    approval_ref       TEXT,
    reason             TEXT,
    -- The source figure as it arrived (a float in the source API) before it was
    -- written as a decimal, so the conversion is always traceable.
    source_amount_raw  TEXT NOT NULL,
    state              TEXT NOT NULL,
    quarantine_reason  TEXT,
    line_hash          TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_in_line_kind_check CHECK (source_kind IN
        ('BASE_SALARY', 'COMP_COMPONENT', 'COMP_BONUS', 'BENEFIT_EMPLOYEE', 'BENEFIT_EMPLOYER')),
    CONSTRAINT wfp_in_line_category_check CHECK (category IN ('EARNING', 'DEDUCTION', 'BENEFIT', 'REIMBURSEMENT', 'ADJUSTMENT')),
    CONSTRAINT wfp_in_line_state_check CHECK (state IN ('ACCEPTED', 'QUARANTINED')),
    CONSTRAINT wfp_in_line_reason_check CHECK ((state = 'QUARANTINED') = (quarantine_reason IS NOT NULL)),
    CONSTRAINT wfp_in_line_amount_check CHECK (amount >= 0),
    -- An ACCEPTED line is fully classified.
    CONSTRAINT wfp_in_line_accepted_complete CHECK (
        state <> 'ACCEPTED' OR (payroll_code IS NOT NULL AND tax_treatment IS NOT NULL AND mapping_id IS NOT NULL
                                AND source_ref IS NOT NULL AND currency ~ '^[A-Z]{3}$')),
    -- A source line is one financial input, however many times it is delivered.
    CONSTRAINT wfp_in_line_unique_source UNIQUE (batch_id, source_line_id)
);
CREATE INDEX IF NOT EXISTS idx_wfp_in_line_batch ON wfp_payroll_input_lines (tenant_id, batch_id, state);
CREATE INDEX IF NOT EXISTS idx_wfp_in_line_worker ON wfp_payroll_input_lines (tenant_id, batch_id, worker_id);
CREATE INDEX IF NOT EXISTS idx_wfp_in_line_source ON wfp_payroll_input_lines (tenant_id, source_line_id);

-- Control totals per category and currency. Never summed across currencies.
CREATE TABLE IF NOT EXISTS wfp_payroll_input_totals (
    tenant_id     TEXT NOT NULL,
    batch_id      UUID NOT NULL REFERENCES wfp_payroll_input_batches (batch_id),
    category      TEXT NOT NULL,
    currency      TEXT NOT NULL,
    line_count    INTEGER NOT NULL,
    total_amount  NUMERIC(18, 4) NOT NULL,
    PRIMARY KEY (batch_id, category, currency)
);

-- ── Guards ───────────────────────────────────────────────────────────────────
-- Mappings are governed evidence: no UPDATE, no DELETE.
CREATE OR REPLACE FUNCTION wfp_in_mapping_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wfp_input_code_mappings is append-only (% blocked)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_in_mapping_no_change ON wfp_input_code_mappings;
CREATE TRIGGER wfp_in_mapping_no_change
    BEFORE UPDATE OR DELETE ON wfp_input_code_mappings
    FOR EACH ROW EXECUTE FUNCTION wfp_in_mapping_immutable();

-- A frozen population is immutable; CONSUMED is the end of the line (a correction
-- after payroll consumed it is a payroll correction, Wave 4). A batch's identity
-- never changes, and nothing is deleted.
CREATE OR REPLACE FUNCTION wfp_in_batch_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payroll input batches are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.payroll_period_id IS DISTINCT FROM OLD.payroll_period_id
       OR NEW.period_start IS DISTINCT FROM OLD.period_start
       OR NEW.period_end IS DISTINCT FROM OLD.period_end
       OR NEW.batch_version IS DISTINCT FROM OLD.batch_version
       OR NEW.supersedes_batch_id IS DISTINCT FROM OLD.supersedes_batch_id THEN
        RAISE EXCEPTION 'batch identity is immutable; a changed population is a new batch version'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('SUPERSEDED', 'REJECTED', 'CANCELLED', 'CONSUMED') AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION '% is terminal', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('FROZEN', 'CONSUMED') AND (
           NEW.worker_count IS DISTINCT FROM OLD.worker_count OR NEW.line_count IS DISTINCT FROM OLD.line_count
        OR NEW.accepted_line_count IS DISTINCT FROM OLD.accepted_line_count
        OR NEW.quarantined_line_count IS DISTINCT FROM OLD.quarantined_line_count
        OR NEW.source_hash IS DISTINCT FROM OLD.source_hash) THEN
        RAISE EXCEPTION 'a FROZEN batch population is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- The state machine itself, enforced here and not only in Go.
    IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
           (OLD.state = 'RECEIVED' AND NEW.state IN ('READY', 'REJECTED'))
        OR (OLD.state = 'READY'    AND NEW.state IN ('FROZEN', 'SUPERSEDED', 'CANCELLED'))
        OR (OLD.state = 'FROZEN'   AND NEW.state IN ('CONSUMED', 'SUPERSEDED'))) THEN
        RAISE EXCEPTION 'illegal input batch transition % -> %', OLD.state, NEW.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.state = 'FROZEN' AND OLD.state NOT IN ('READY', 'FROZEN') THEN
        RAISE EXCEPTION 'only a READY batch can be frozen' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.state = 'CONSUMED' AND OLD.state NOT IN ('FROZEN', 'CONSUMED') THEN
        RAISE EXCEPTION 'only a FROZEN batch can be consumed by a payroll run' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_in_batch_guard_trg ON wfp_payroll_input_batches;
CREATE TRIGGER wfp_in_batch_guard_trg
    BEFORE UPDATE OR DELETE ON wfp_payroll_input_batches
    FOR EACH ROW EXECUTE FUNCTION wfp_in_batch_guard();

-- Lines and totals are written only while their batch is RECEIVED (the creating
-- transaction) and are never edited or deleted afterwards.
CREATE OR REPLACE FUNCTION wfp_in_child_guard() RETURNS trigger AS $$
DECLARE b_state TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION '% rows are immutable (% blocked)', TG_TABLE_NAME, TG_OP
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT state INTO b_state FROM wfp_payroll_input_batches WHERE batch_id = NEW.batch_id;
    IF b_state IS DISTINCT FROM 'RECEIVED' THEN
        RAISE EXCEPTION '% can only be written while the batch is RECEIVED', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_in_line_guard_trg ON wfp_payroll_input_lines;
CREATE TRIGGER wfp_in_line_guard_trg
    BEFORE INSERT OR UPDATE OR DELETE ON wfp_payroll_input_lines
    FOR EACH ROW EXECUTE FUNCTION wfp_in_child_guard();
DROP TRIGGER IF EXISTS wfp_in_totals_guard_trg ON wfp_payroll_input_totals;
CREATE TRIGGER wfp_in_totals_guard_trg
    BEFORE INSERT OR UPDATE OR DELETE ON wfp_payroll_input_totals
    FOR EACH ROW EXECUTE FUNCTION wfp_in_child_guard();

-- ── Row-level security ───────────────────────────────────────────────────────
ALTER TABLE wfp_input_code_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_input_code_mappings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_input_code_mappings FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payroll_input_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_input_batches FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_input_batches FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payroll_input_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_input_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_input_lines FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payroll_input_totals ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_input_totals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_input_totals FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
