-- ZS-SVC-L-001 Wave 8 — Payroll reconciliation, Banking outcome evidence, close gate.
--
-- Decisions (2026-10-15): the exception register lives in payroll-run-svc; Banking outcomes are
-- observed by a refresh job and raise exceptions, they never reopen anything by themselves;
-- reopening (RETURNED) is an explicit authorised command.

-- ── Banking observations (append-only evidence of what Banking reported) ─────
CREATE TABLE IF NOT EXISTS wfp_instruction_banking_observations (
    observation_id  UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    legal_entity_id TEXT NOT NULL,
    instruction_id  UUID NOT NULL REFERENCES wfp_payment_instructions (instruction_id),
    source          TEXT NOT NULL,
    raw_status      TEXT NOT NULL,
    outcome         TEXT NOT NULL,
    attempt_id      TEXT,
    payment_id      TEXT,
    detail          TEXT,
    observed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_ibo_source CHECK (source IN ('ADAPTER_ATTEMPT', 'PAYMENT_STATUS', 'HANDOFF')),
    CONSTRAINT wfp_ibo_outcome CHECK (outcome IN ('IN_FLIGHT', 'UNRESOLVED', 'SETTLED', 'RETURNED', 'FAILED'))
);
CREATE INDEX IF NOT EXISTS idx_wfp_ibo_latest ON wfp_instruction_banking_observations (instruction_id, observed_at DESC);

-- ── Reconciliation runs (frozen population + assertion results) ──────────────
CREATE TABLE IF NOT EXISTS wfp_reconciliation_runs (
    recon_run_id     UUID PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    legal_entity_id  TEXT NOT NULL,
    run_ref_id       UUID NOT NULL REFERENCES wfp_payroll_run_refs (run_ref_id),
    result_id        UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    outcome          TEXT NOT NULL,
    population       JSONB NOT NULL,
    population_hash  TEXT NOT NULL,
    assertions       JSONB NOT NULL,
    exception_count  INTEGER NOT NULL DEFAULT 0,
    executed_by      TEXT NOT NULL,
    executed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    reperformed_from UUID REFERENCES wfp_reconciliation_runs (recon_run_id),
    certified_by     TEXT,
    certified_at     TIMESTAMPTZ,
    correlation_id   TEXT NOT NULL,
    CONSTRAINT wfp_rr_outcome CHECK (outcome IN ('MATCHED', 'BREAK')),
    CONSTRAINT wfp_rr_certified CHECK ((certified_by IS NULL) = (certified_at IS NULL)),
    CONSTRAINT wfp_rr_certify_matched CHECK (certified_by IS NULL OR outcome = 'MATCHED'),
    CONSTRAINT wfp_rr_sod CHECK (certified_by IS NULL OR certified_by <> executed_by)
);
CREATE INDEX IF NOT EXISTS idx_wfp_rr_result ON wfp_reconciliation_runs (tenant_id, result_id, executed_at DESC);

-- ── Reconciliation exceptions (close only when the cause clears, or by evidenced waiver) ──
CREATE TABLE IF NOT EXISTS wfp_reconciliation_exceptions (
    exception_id         UUID PRIMARY KEY,
    tenant_id            TEXT NOT NULL,
    legal_entity_id      TEXT NOT NULL,
    run_ref_id           UUID NOT NULL REFERENCES wfp_payroll_run_refs (run_ref_id),
    result_id            UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    recon_run_id         UUID NOT NULL REFERENCES wfp_reconciliation_runs (recon_run_id),
    code                 TEXT NOT NULL,
    subject_key          TEXT NOT NULL,
    severity             TEXT NOT NULL DEFAULT 'BLOCKING',
    amount               NUMERIC(18, 4),
    detail               TEXT NOT NULL,
    state                TEXT NOT NULL DEFAULT 'OPEN',
    owner                TEXT,
    resolution           TEXT,
    resolved_at          TIMESTAMPTZ,
    waived_by            TEXT,
    waiver_reason        TEXT,
    waiver_evidence_ref  TEXT,
    raised_by            TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_re_state CHECK (state IN ('OPEN', 'ASSIGNED', 'RESOLVED', 'WAIVED')),
    CONSTRAINT wfp_re_severity CHECK (severity IN ('BLOCKING', 'WARNING')),
    CONSTRAINT wfp_re_resolved CHECK ((state IN ('RESOLVED', 'WAIVED')) = (resolved_at IS NOT NULL)),
    CONSTRAINT wfp_re_waiver CHECK (state <> 'WAIVED' OR (waived_by IS NOT NULL AND length(btrim(waiver_reason)) > 0
        AND length(btrim(waiver_evidence_ref)) > 0 AND waived_by <> raised_by)),
    CONSTRAINT wfp_re_assigned CHECK (state <> 'ASSIGNED' OR owner IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_wfp_re_open ON wfp_reconciliation_exceptions (tenant_id, result_id, code, subject_key)
    WHERE state IN ('OPEN', 'ASSIGNED');
CREATE INDEX IF NOT EXISTS idx_wfp_re_gate ON wfp_reconciliation_exceptions (tenant_id, legal_entity_id)
    WHERE state IN ('OPEN', 'ASSIGNED') AND severity = 'BLOCKING';

-- ── Instructions: a returned payment, and the payment linked to an instruction ──
ALTER TABLE wfp_payment_instructions ADD COLUMN IF NOT EXISTS payment_id    TEXT;
ALTER TABLE wfp_payment_instructions ADD COLUMN IF NOT EXISTS banking_checked_at TIMESTAMPTZ;
ALTER TABLE wfp_payment_instructions ADD COLUMN IF NOT EXISTS returned_at   TIMESTAMPTZ;
ALTER TABLE wfp_payment_instructions ADD COLUMN IF NOT EXISTS return_reason TEXT;
ALTER TABLE wfp_payment_instructions DROP CONSTRAINT IF EXISTS wfp_pi_state_check;
ALTER TABLE wfp_payment_instructions ADD CONSTRAINT wfp_pi_state_check CHECK (state IN
    ('DRAFT', 'VALIDATED', 'APPROVED', 'SUBMITTED_TO_BANKING', 'HANDED_OFF', 'CANCELLED', 'SUPERSEDED', 'RETURNED'));
-- A RETURNED instruction was handed off: it keeps its handoff time.
ALTER TABLE wfp_payment_instructions DROP CONSTRAINT IF EXISTS wfp_pi_handed_off;
ALTER TABLE wfp_payment_instructions ADD CONSTRAINT wfp_pi_handed_off CHECK (
    (state <> 'HANDED_OFF' OR handed_off_at IS NOT NULL) AND (handed_off_at IS NULL OR state IN ('HANDED_OFF', 'RETURNED')));
ALTER TABLE wfp_payment_instructions DROP CONSTRAINT IF EXISTS wfp_pi_returned;
ALTER TABLE wfp_payment_instructions ADD CONSTRAINT wfp_pi_returned CHECK (
    (state = 'RETURNED') = (returned_at IS NOT NULL) AND (state <> 'RETURNED' OR return_reason IS NOT NULL));

-- A returned instruction frees its slot: a new net-pay instruction for the worker may be raised.
DROP INDEX IF EXISTS uq_wfp_pi_net_pay;
CREATE UNIQUE INDEX uq_wfp_pi_net_pay ON wfp_payment_instructions (tenant_id, result_id, worker_id)
    WHERE instruction_type = 'NET_PAY' AND state NOT IN ('CANCELLED', 'SUPERSEDED', 'RETURNED');

CREATE OR REPLACE FUNCTION wfp_pi_update_guard() RETURNS trigger AS $$
DECLARE r_state TEXT; valid_approvals INTEGER; latest_outcome TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment instructions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.instruction_type IS DISTINCT FROM OLD.instruction_type OR NEW.run_ref_id IS DISTINCT FROM OLD.run_ref_id
       OR NEW.result_id IS DISTINCT FROM OLD.result_id OR NEW.worker_id IS DISTINCT FROM OLD.worker_id
       OR NEW.liability_id IS DISTINCT FROM OLD.liability_id OR NEW.created_by IS DISTINCT FROM OLD.created_by
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key THEN
        RAISE EXCEPTION 'a payment instruction''s identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.state <> 'DRAFT' AND (
           NEW.payee_ref IS DISTINCT FROM OLD.payee_ref OR NEW.destination_id IS DISTINCT FROM OLD.destination_id
        OR NEW.destination_version IS DISTINCT FROM OLD.destination_version OR NEW.destination_fingerprint IS DISTINCT FROM OLD.destination_fingerprint
        OR NEW.amount IS DISTINCT FROM OLD.amount OR NEW.currency IS DISTINCT FROM OLD.currency OR NEW.value_date IS DISTINCT FROM OLD.value_date
        OR NEW.payer_account_ref IS DISTINCT FROM OLD.payer_account_ref OR NEW.payment_method IS DISTINCT FROM OLD.payment_method
        OR NEW.payment_reference IS DISTINCT FROM OLD.payment_reference OR NEW.fingerprint IS DISTINCT FROM OLD.fingerprint) THEN
        RAISE EXCEPTION 'a validated payment instruction is frozen; invalidate the approval to change it'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- The Banking payment linked to an instruction is set once and never changes.
    IF OLD.payment_id IS NOT NULL AND NEW.payment_id IS DISTINCT FROM OLD.payment_id THEN
        RAISE EXCEPTION 'the payment linked to an instruction never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
           (OLD.state = 'DRAFT'                AND NEW.state IN ('VALIDATED', 'CANCELLED', 'SUPERSEDED'))
        OR (OLD.state = 'VALIDATED'            AND NEW.state IN ('APPROVED', 'DRAFT', 'CANCELLED', 'SUPERSEDED'))
        OR (OLD.state = 'APPROVED'             AND NEW.state IN ('SUBMITTED_TO_BANKING', 'DRAFT', 'CANCELLED', 'SUPERSEDED'))
        OR (OLD.state = 'SUBMITTED_TO_BANKING' AND NEW.state = 'HANDED_OFF')
        OR (OLD.state = 'SUBMITTED_TO_BANKING' AND NEW.state = 'APPROVED' AND OLD.attempt_id IS NULL)
        OR (OLD.state = 'HANDED_OFF'           AND NEW.state = 'RETURNED')) THEN
        RAISE EXCEPTION 'illegal payment instruction transition % -> %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- RETURNED is a Banking fact: allowed only when the latest Banking observation says RETURNED.
    IF NEW.state = 'RETURNED' AND OLD.state IS DISTINCT FROM 'RETURNED' THEN
        SELECT outcome INTO latest_outcome FROM wfp_instruction_banking_observations
            WHERE instruction_id = NEW.instruction_id ORDER BY observed_at DESC, observation_id DESC LIMIT 1;
        IF latest_outcome IS DISTINCT FROM 'RETURNED' THEN
            RAISE EXCEPTION 'an instruction is marked returned only when Banking reported the payment RETURNED'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state AND NEW.state IN ('APPROVED', 'SUBMITTED_TO_BANKING') THEN
        SELECT acceptance_state INTO r_state FROM wfp_payroll_run_refs WHERE run_ref_id = NEW.run_ref_id;
        IF r_state IS DISTINCT FROM 'ACCEPTED' THEN
            RAISE EXCEPTION 'the payroll result behind this instruction is no longer ACCEPTED' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    IF NEW.state = 'APPROVED' AND OLD.state IS DISTINCT FROM 'APPROVED' THEN
        SELECT count(DISTINCT approver) INTO valid_approvals FROM wfp_payment_approvals
            WHERE instruction_id = NEW.instruction_id AND invalidated_at IS NULL AND fingerprint = NEW.fingerprint;
        IF valid_approvals < NEW.required_signatures THEN
            RAISE EXCEPTION 'approval quorum not met (% of % signatures)', valid_approvals, NEW.required_signatures
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    IF OLD.state IN ('SUBMITTED_TO_BANKING', 'HANDED_OFF') AND OLD.attempt_id IS NOT NULL AND NEW.attempt_id IS DISTINCT FROM OLD.attempt_id THEN
        RAISE EXCEPTION 'the Banking attempt of an instruction never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── Guards ───────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION wfp_ibo_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'Banking observations are append-only (% blocked)', TG_OP USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_ibo_append_only_trg ON wfp_instruction_banking_observations;
CREATE TRIGGER wfp_ibo_append_only_trg BEFORE UPDATE OR DELETE ON wfp_instruction_banking_observations
    FOR EACH ROW EXECUTE FUNCTION wfp_ibo_append_only();

-- A reconciliation run is evidence: only its certification can be set, once.
CREATE OR REPLACE FUNCTION wfp_rr_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation runs are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.recon_run_id IS DISTINCT FROM OLD.recon_run_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.result_id IS DISTINCT FROM OLD.result_id OR NEW.outcome IS DISTINCT FROM OLD.outcome
       OR NEW.population IS DISTINCT FROM OLD.population OR NEW.population_hash IS DISTINCT FROM OLD.population_hash
       OR NEW.assertions IS DISTINCT FROM OLD.assertions OR NEW.executed_by IS DISTINCT FROM OLD.executed_by THEN
        RAISE EXCEPTION 'a reconciliation run is immutable evidence; reperform it instead' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.certified_by IS NOT NULL THEN
        RAISE EXCEPTION 'a certified reconciliation run is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_rr_guard_trg ON wfp_reconciliation_runs;
CREATE TRIGGER wfp_rr_guard_trg BEFORE UPDATE OR DELETE ON wfp_reconciliation_runs FOR EACH ROW EXECUTE FUNCTION wfp_rr_guard();

-- Exceptions: never deleted, identity immutable, RESOLVED and WAIVED are terminal.
CREATE OR REPLACE FUNCTION wfp_re_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation exceptions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.result_id IS DISTINCT FROM OLD.result_id
       OR NEW.code IS DISTINCT FROM OLD.code OR NEW.subject_key IS DISTINCT FROM OLD.subject_key
       OR NEW.detail IS DISTINCT FROM OLD.detail OR NEW.raised_by IS DISTINCT FROM OLD.raised_by
       OR NEW.recon_run_id IS DISTINCT FROM OLD.recon_run_id THEN
        RAISE EXCEPTION 'an exception''s identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('RESOLVED', 'WAIVED') AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION 'a % exception is terminal', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_re_guard_trg ON wfp_reconciliation_exceptions;
CREATE TRIGGER wfp_re_guard_trg BEFORE UPDATE OR DELETE ON wfp_reconciliation_exceptions FOR EACH ROW EXECUTE FUNCTION wfp_re_guard();

-- ── Row-level security ───────────────────────────────────────────────────────
ALTER TABLE wfp_instruction_banking_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_instruction_banking_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_instruction_banking_observations FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_reconciliation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_reconciliation_runs FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_reconciliation_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_reconciliation_exceptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_reconciliation_exceptions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- ── Background loops (Banking refresh, liability DUE sweep) ──────────────────
-- Like the posting dispatcher they are not requests: they install app.outbox_relay to find
-- work across tenants, then switch to each item's own tenant before writing anything.
DROP POLICY IF EXISTS tenant_isolation_policy ON wfp_payment_instructions;
CREATE POLICY tenant_isolation_policy ON wfp_payment_instructions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true')
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true');

DROP POLICY IF EXISTS tenant_isolation_policy ON wfp_instruction_banking_observations;
CREATE POLICY tenant_isolation_policy ON wfp_instruction_banking_observations FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true')
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true');

DROP POLICY IF EXISTS tenant_isolation_policy ON wfp_payroll_liabilities;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_liabilities FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true')
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true');
