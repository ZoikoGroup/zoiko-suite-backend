-- ZS-SVC-L-001 WFP-04 — Payroll Result Interface.
--
-- An accepted gross-to-net result received from the payroll engine, with its
-- worker-level and component-level detail. This service does not calculate pay:
-- it validates that a source result is final, rule-backed, internally consistent
-- and mapped to real workers, then keeps it as immutable evidence. A correction
-- is a NEW source run version that supersedes the original through a chain; the
-- original is never edited.
--
-- Decisions (2026-10-14): one ReceivePayrollResult command for both the Zoiko path
-- and an external provider; control totals are EXACT (no tolerance).

-- ── Results ──────────────────────────────────────────────────────────────────
-- One row per run reference (a run version has exactly one result, ACCEPTED or
-- REJECTED). A REJECTED row keeps the reasons and totals as evidence for the
-- investigation (WFP-RB-04) but no worker detail.
CREATE TABLE IF NOT EXISTS wfp_payroll_results (
    result_id                   UUID PRIMARY KEY,
    tenant_id                   TEXT NOT NULL,
    run_ref_id                  UUID NOT NULL UNIQUE REFERENCES wfp_payroll_run_refs (run_ref_id),
    legal_entity_id             TEXT NOT NULL,
    outcome                     TEXT NOT NULL,
    rejection_reasons           JSONB NOT NULL DEFAULT '[]'::jsonb,
    currency                    TEXT NOT NULL,
    source_run_status           TEXT NOT NULL,
    source_approval_ref         TEXT,
    calculation_ref             TEXT,
    input_batch_id              UUID REFERENCES wfp_payroll_input_batches (batch_id),
    -- Honest labels on what the source did NOT establish (e.g. PLACEHOLDER_TAX_RATE).
    evidence_flags              TEXT[] NOT NULL DEFAULT '{}',
    worker_count                INTEGER NOT NULL,
    total_gross                 NUMERIC(18, 4) NOT NULL,
    total_taxable_pay           NUMERIC(18, 4) NOT NULL,
    total_employee_deductions   NUMERIC(18, 4) NOT NULL,
    total_employer_liabilities  NUMERIC(18, 4) NOT NULL,
    total_net_pay               NUMERIC(18, 4) NOT NULL,
    declared_totals             JSONB NOT NULL,
    result_fingerprint          TEXT NOT NULL,
    received_by                 TEXT NOT NULL,
    correlation_id              TEXT NOT NULL,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_result_outcome_check CHECK (outcome IN ('ACCEPTED', 'REJECTED')),
    CONSTRAINT wfp_result_reasons_check CHECK ((outcome = 'REJECTED') = (jsonb_array_length(rejection_reasons) > 0)),
    CONSTRAINT wfp_result_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    -- An ACCEPTED result's declared totals ARE its computed totals; the check is
    -- repeated at the database so a broken population cannot be stored as accepted.
    CONSTRAINT wfp_result_accepted_reconciles CHECK (
        outcome <> 'ACCEPTED' OR (total_gross - total_employee_deductions = total_net_pay AND source_run_status IN ('FINAL', 'APPROVED')
                                  AND source_approval_ref IS NOT NULL AND calculation_ref IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS wfp_payroll_result_workers (
    tenant_id             TEXT NOT NULL,
    result_id             UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    worker_id             UUID NOT NULL,
    gross                 NUMERIC(18, 4) NOT NULL,
    taxable_pay           NUMERIC(18, 4) NOT NULL,
    employee_deductions   NUMERIC(18, 4) NOT NULL,
    employer_liabilities  NUMERIC(18, 4) NOT NULL,
    net_pay               NUMERIC(18, 4) NOT NULL,
    currency              TEXT NOT NULL,
    PRIMARY KEY (result_id, worker_id),
    CONSTRAINT wfp_result_worker_net CHECK (gross - employee_deductions = net_pay),
    CONSTRAINT wfp_result_worker_nonneg CHECK (gross >= 0 AND taxable_pay >= 0 AND employee_deductions >= 0
                                               AND employer_liabilities >= 0 AND net_pay >= 0)
);
CREATE INDEX IF NOT EXISTS idx_wfp_result_worker ON wfp_payroll_result_workers (tenant_id, worker_id);

CREATE TABLE IF NOT EXISTS wfp_payroll_result_components (
    component_id   UUID PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    result_id      UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    worker_id      UUID NOT NULL,
    sequence       INTEGER NOT NULL,
    code           TEXT NOT NULL,
    kind           TEXT NOT NULL,
    amount         NUMERIC(18, 4) NOT NULL,
    -- For liability components: the authority/beneficiary the amount is owed to (WFP-06).
    authority_ref  TEXT,
    CONSTRAINT wfp_result_comp_kind CHECK (kind IN
        ('EARNING', 'EMPLOYEE_DEDUCTION', 'EMPLOYEE_TAX', 'EMPLOYEE_SOCIAL', 'EMPLOYEE_PENSION', 'EMPLOYEE_BENEFIT',
         'EMPLOYER_TAX', 'EMPLOYER_SOCIAL', 'EMPLOYER_PENSION', 'EMPLOYER_BENEFIT', 'EMPLOYER_OTHER')),
    CONSTRAINT wfp_result_comp_amount CHECK (amount >= 0),
    FOREIGN KEY (result_id, worker_id) REFERENCES wfp_payroll_result_workers (result_id, worker_id),
    CONSTRAINT wfp_result_comp_unique UNIQUE (result_id, worker_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_wfp_result_comp ON wfp_payroll_result_components (tenant_id, result_id, worker_id);

-- ── Correction chain (WFP-CTRL-024) ──────────────────────────────────────────
-- Original result -> superseding result, with why. The original is kept as it was.
CREATE TABLE IF NOT EXISTS wfp_payroll_correction_chains (
    chain_id                 UUID PRIMARY KEY,
    tenant_id                TEXT NOT NULL,
    original_run_ref_id      UUID NOT NULL REFERENCES wfp_payroll_run_refs (run_ref_id),
    superseding_run_ref_id   UUID NOT NULL UNIQUE REFERENCES wfp_payroll_run_refs (run_ref_id),
    original_result_id       UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    superseding_result_id    UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    reason                   TEXT NOT NULL,
    created_by               TEXT NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_chain_distinct CHECK (original_run_ref_id <> superseding_run_ref_id)
);

-- ── Guards ───────────────────────────────────────────────────────────────────
-- A result is written only while its run reference is being VALIDATED, and is
-- never changed or deleted afterwards.
CREATE OR REPLACE FUNCTION wfp_result_insert_guard() RETURNS trigger AS $$
DECLARE r_state TEXT; r_outcome TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION '% rows are immutable (% blocked)', TG_TABLE_NAME, TG_OP
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_TABLE_NAME = 'wfp_payroll_results' THEN
        SELECT acceptance_state INTO r_state FROM wfp_payroll_run_refs WHERE run_ref_id = NEW.run_ref_id;
    ELSE
        SELECT r.acceptance_state, res.outcome INTO r_state, r_outcome
        FROM wfp_payroll_results res JOIN wfp_payroll_run_refs r ON r.run_ref_id = res.run_ref_id
        WHERE res.result_id = NEW.result_id;
        IF r_outcome IS DISTINCT FROM 'ACCEPTED' THEN
            RAISE EXCEPTION 'worker and component detail belongs only to an ACCEPTED result'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    IF r_state IS DISTINCT FROM 'VALIDATING' THEN
        RAISE EXCEPTION '% can only be written while the payroll run is VALIDATING (is %)', TG_TABLE_NAME, r_state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wfp_result_guard_trg ON wfp_payroll_results;
CREATE TRIGGER wfp_result_guard_trg BEFORE INSERT OR UPDATE OR DELETE ON wfp_payroll_results
    FOR EACH ROW EXECUTE FUNCTION wfp_result_insert_guard();
DROP TRIGGER IF EXISTS wfp_result_workers_guard_trg ON wfp_payroll_result_workers;
CREATE TRIGGER wfp_result_workers_guard_trg BEFORE INSERT OR UPDATE OR DELETE ON wfp_payroll_result_workers
    FOR EACH ROW EXECUTE FUNCTION wfp_result_insert_guard();
DROP TRIGGER IF EXISTS wfp_result_components_guard_trg ON wfp_payroll_result_components;
CREATE TRIGGER wfp_result_components_guard_trg BEFORE INSERT OR UPDATE OR DELETE ON wfp_payroll_result_components
    FOR EACH ROW EXECUTE FUNCTION wfp_result_insert_guard();

-- A chain link is written only for a real supersession: the original must be
-- ACCEPTED, and the superseding result an ACCEPTED one still being validated.
CREATE OR REPLACE FUNCTION wfp_chain_guard() RETURNS trigger AS $$
DECLARE o_state TEXT; s_state TEXT; s_outcome TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'correction chain rows are immutable (% blocked)', TG_OP
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT acceptance_state INTO o_state FROM wfp_payroll_run_refs WHERE run_ref_id = NEW.original_run_ref_id;
    SELECT r.acceptance_state, res.outcome INTO s_state, s_outcome
        FROM wfp_payroll_run_refs r JOIN wfp_payroll_results res ON res.run_ref_id = r.run_ref_id
        WHERE r.run_ref_id = NEW.superseding_run_ref_id AND res.result_id = NEW.superseding_result_id;
    IF o_state IS DISTINCT FROM 'ACCEPTED' OR s_state IS DISTINCT FROM 'VALIDATING' OR s_outcome IS DISTINCT FROM 'ACCEPTED' THEN
        RAISE EXCEPTION 'a correction chain links an ACCEPTED original to an ACCEPTED superseding result being validated'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_chain_guard_trg ON wfp_payroll_correction_chains;
CREATE TRIGGER wfp_chain_guard_trg BEFORE INSERT OR UPDATE OR DELETE ON wfp_payroll_correction_chains
    FOR EACH ROW EXECUTE FUNCTION wfp_chain_guard();

-- The Wave 1 run-reference guard, extended: a run version cannot become ACCEPTED
-- without an ACCEPTED result carrying the same fingerprint, REJECTED without a
-- REJECTED result, or SUPERSEDED without a correction chain (the result and its
-- correction chain are the acceptance evidence).
CREATE OR REPLACE FUNCTION wfp_run_ref_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wfp_payroll_run_refs rows are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.source_system_id IS DISTINCT FROM OLD.source_system_id
       OR NEW.source_run_id IS DISTINCT FROM OLD.source_run_id
       OR NEW.source_run_version IS DISTINCT FROM OLD.source_run_version
       OR NEW.run_kind IS DISTINCT FROM OLD.run_kind
       OR NEW.payroll_period_id IS DISTINCT FROM OLD.payroll_period_id
       OR NEW.pay_date IS DISTINCT FROM OLD.pay_date
       OR NEW.rule_manifest_id IS DISTINCT FROM OLD.rule_manifest_id
       OR NEW.identity_fingerprint IS DISTINCT FROM OLD.identity_fingerprint
       OR NEW.supersedes_run_ref_id IS DISTINCT FROM OLD.supersedes_run_ref_id THEN
        RAISE EXCEPTION 'payroll run identity is immutable; a changed run is a new source_run_version'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.result_fingerprint IS NOT NULL AND NEW.result_fingerprint IS DISTINCT FROM OLD.result_fingerprint THEN
        RAISE EXCEPTION 'result_fingerprint is set once'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.acceptance_state = 'ACCEPTED' AND NEW.acceptance_state NOT IN ('ACCEPTED', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'an ACCEPTED payroll result is immutable; it can only be superseded'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.acceptance_state IN ('REJECTED', 'SUPERSEDED') AND NEW.acceptance_state IS DISTINCT FROM OLD.acceptance_state THEN
        RAISE EXCEPTION '% is terminal', OLD.acceptance_state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.acceptance_state IS DISTINCT FROM OLD.acceptance_state THEN
        IF NEW.acceptance_state = 'ACCEPTED' AND NOT EXISTS (
               SELECT 1 FROM wfp_payroll_results r
               WHERE r.run_ref_id = NEW.run_ref_id AND r.outcome = 'ACCEPTED' AND r.result_fingerprint = NEW.result_fingerprint) THEN
            RAISE EXCEPTION 'a payroll run is ACCEPTED only with an ACCEPTED result carrying its fingerprint'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.acceptance_state = 'REJECTED' AND OLD.acceptance_state = 'VALIDATING' AND NOT EXISTS (
               SELECT 1 FROM wfp_payroll_results r WHERE r.run_ref_id = NEW.run_ref_id AND r.outcome = 'REJECTED') THEN
            RAISE EXCEPTION 'a payroll run is REJECTED after validation only with a REJECTED result as evidence'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.acceptance_state = 'SUPERSEDED' AND NOT EXISTS (
               SELECT 1 FROM wfp_payroll_correction_chains c WHERE c.original_run_ref_id = NEW.run_ref_id) THEN
            RAISE EXCEPTION 'a payroll result is SUPERSEDED only through a correction chain'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── Row-level security ───────────────────────────────────────────────────────
ALTER TABLE wfp_payroll_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_results FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_results FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payroll_result_workers ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_result_workers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_result_workers FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payroll_result_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_result_components FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_result_components FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payroll_correction_chains ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payroll_correction_chains FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_correction_chains FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
