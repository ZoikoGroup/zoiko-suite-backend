-- ZS-SVC-L-001 WFP-07 — Payroll Payment Interface.
--
-- A PayrollPaymentInstruction is an approved request for Banking to pay a worker's net
-- pay or an authority's liability. WFP-07 owns the instruction subject, its approval
-- fingerprint, the source links and the Banking handoff references. It does NOT own the
-- beneficiary bank master (payee-banking-identity-svc), the provider message, payment
-- execution, settlement or finality (Banking), or the payroll calculation.
--
-- Decisions (2026-10-14): the object lives here; the beneficiary is the ACTIVE destination of
-- the payee, pinned by id and version; scope is NET_PAY and LIABILITY; approval is
-- maker-checker with a policy-driven signature quorum and a separate explicit submit.

CREATE TABLE IF NOT EXISTS wfp_payment_instructions (
    instruction_id          UUID PRIMARY KEY,
    tenant_id               TEXT NOT NULL,
    legal_entity_id         TEXT NOT NULL,
    instruction_type        TEXT NOT NULL,
    run_ref_id              UUID NOT NULL REFERENCES wfp_payroll_run_refs (run_ref_id),
    result_id               UUID NOT NULL REFERENCES wfp_payroll_results (result_id),
    worker_id               UUID,
    liability_id            UUID REFERENCES wfp_payroll_liabilities (liability_id),
    payee_ref               TEXT NOT NULL,
    destination_id          TEXT NOT NULL,
    -- The destination's version (its updated_at) and bank-detail digest when it was pinned.
    destination_version     TEXT NOT NULL,
    destination_fingerprint TEXT NOT NULL,
    amount                  NUMERIC(18, 4) NOT NULL,
    currency                TEXT NOT NULL,
    value_date              DATE NOT NULL,
    payer_account_ref       TEXT NOT NULL,
    payment_method          TEXT NOT NULL DEFAULT 'BANK_TRANSFER',
    payment_reference       TEXT NOT NULL,
    -- Binds result, subject, payee version, amount, currency, date and paying-account context.
    fingerprint             TEXT NOT NULL,
    state                   TEXT NOT NULL DEFAULT 'DRAFT',
    policy_result           TEXT,
    policy_version_id       TEXT,
    required_signatures     INTEGER NOT NULL DEFAULT 1,
    approved_fingerprint    TEXT,
    approved_at             TIMESTAMPTZ,
    invalidation_reason     TEXT,
    invalidated_at          TIMESTAMPTZ,
    cancel_reason           TEXT,
    attempt_id              TEXT,
    banking_status          TEXT,
    banking_error           TEXT,
    submitted_by            TEXT,
    submitted_at            TIMESTAMPTZ,
    handed_off_at           TIMESTAMPTZ,
    idempotency_key         TEXT,
    version                 INTEGER NOT NULL DEFAULT 1,
    correlation_id          TEXT NOT NULL,
    created_by              TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT wfp_pi_type_check CHECK (instruction_type IN ('NET_PAY', 'LIABILITY')),
    CONSTRAINT wfp_pi_subject_check CHECK (
        (instruction_type = 'NET_PAY' AND worker_id IS NOT NULL AND liability_id IS NULL)
        OR (instruction_type = 'LIABILITY' AND liability_id IS NOT NULL AND worker_id IS NULL)),
    CONSTRAINT wfp_pi_state_check CHECK (state IN
        ('DRAFT', 'VALIDATED', 'APPROVED', 'SUBMITTED_TO_BANKING', 'HANDED_OFF', 'CANCELLED', 'SUPERSEDED')),
    CONSTRAINT wfp_pi_amount_check CHECK (amount > 0),
    CONSTRAINT wfp_pi_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wfp_pi_signatures_check CHECK (required_signatures BETWEEN 1 AND 5),
    -- An approved (or later) instruction is approved at exactly the fingerprint it now carries.
    CONSTRAINT wfp_pi_approved_fp CHECK (
        state NOT IN ('APPROVED', 'SUBMITTED_TO_BANKING', 'HANDED_OFF') OR approved_fingerprint = fingerprint),
    CONSTRAINT wfp_pi_submitted CHECK (
        state NOT IN ('SUBMITTED_TO_BANKING', 'HANDED_OFF') OR (submitted_by IS NOT NULL AND submitted_at IS NOT NULL)),
    CONSTRAINT wfp_pi_handed_off CHECK ((state = 'HANDED_OFF') = (handed_off_at IS NOT NULL)),
    CONSTRAINT wfp_pi_cancel_reason CHECK (state <> 'CANCELLED' OR cancel_reason IS NOT NULL)
);
-- One live instruction per worker per result.
CREATE UNIQUE INDEX IF NOT EXISTS uq_wfp_pi_net_pay ON wfp_payment_instructions (tenant_id, result_id, worker_id)
    WHERE instruction_type = 'NET_PAY' AND state NOT IN ('CANCELLED', 'SUPERSEDED');
CREATE UNIQUE INDEX IF NOT EXISTS uq_wfp_pi_idem ON wfp_payment_instructions (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_wfp_pi_result ON wfp_payment_instructions (tenant_id, result_id);
CREATE INDEX IF NOT EXISTS idx_wfp_pi_liability ON wfp_payment_instructions (tenant_id, liability_id) WHERE liability_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_wfp_pi_pending ON wfp_payment_instructions (tenant_id, legal_entity_id, state)
    WHERE state IN ('DRAFT', 'VALIDATED', 'APPROVED', 'SUBMITTED_TO_BANKING');

-- Approvals (a quorum can be required). One valid approval per principal per instruction.
CREATE TABLE IF NOT EXISTS wfp_payment_approvals (
    approval_id     UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    instruction_id  UUID NOT NULL REFERENCES wfp_payment_instructions (instruction_id),
    approver        TEXT NOT NULL,
    fingerprint     TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    invalidated_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_wfp_pa_valid ON wfp_payment_approvals (instruction_id, approver) WHERE invalidated_at IS NULL;

-- A settlement can now be bound to the approved instruction that paid it, which makes its
-- amount an approved, fingerprinted figure instead of the caller's declaration.
ALTER TABLE wfp_liability_settlements ADD COLUMN IF NOT EXISTS instruction_id UUID REFERENCES wfp_payment_instructions (instruction_id);

-- ── Guards ───────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION wfp_pi_insert_guard() RETURNS trigger AS $$
DECLARE r_state TEXT; res_outcome TEXT;
BEGIN
    SELECT acceptance_state INTO r_state FROM wfp_payroll_run_refs WHERE run_ref_id = NEW.run_ref_id;
    SELECT outcome INTO res_outcome FROM wfp_payroll_results WHERE result_id = NEW.result_id AND run_ref_id = NEW.run_ref_id;
    IF res_outcome IS DISTINCT FROM 'ACCEPTED' OR r_state IS DISTINCT FROM 'ACCEPTED' THEN
        RAISE EXCEPTION 'a payment instruction is created only from an ACCEPTED result (run is %)', r_state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.state <> 'DRAFT' OR NEW.approved_fingerprint IS NOT NULL THEN
        RAISE EXCEPTION 'a payment instruction starts as an unapproved DRAFT' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_pi_insert_guard_trg ON wfp_payment_instructions;
CREATE TRIGGER wfp_pi_insert_guard_trg BEFORE INSERT ON wfp_payment_instructions FOR EACH ROW EXECUTE FUNCTION wfp_pi_insert_guard();

CREATE OR REPLACE FUNCTION wfp_pi_update_guard() RETURNS trigger AS $$
DECLARE r_state TEXT; valid_approvals INTEGER;
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

    -- What is paid, to whom, from where, and when is frozen the moment it leaves DRAFT.
    IF OLD.state <> 'DRAFT' AND (
           NEW.payee_ref IS DISTINCT FROM OLD.payee_ref OR NEW.destination_id IS DISTINCT FROM OLD.destination_id
        OR NEW.destination_version IS DISTINCT FROM OLD.destination_version OR NEW.destination_fingerprint IS DISTINCT FROM OLD.destination_fingerprint
        OR NEW.amount IS DISTINCT FROM OLD.amount OR NEW.currency IS DISTINCT FROM OLD.currency OR NEW.value_date IS DISTINCT FROM OLD.value_date
        OR NEW.payer_account_ref IS DISTINCT FROM OLD.payer_account_ref OR NEW.payment_method IS DISTINCT FROM OLD.payment_method
        OR NEW.payment_reference IS DISTINCT FROM OLD.payment_reference OR NEW.fingerprint IS DISTINCT FROM OLD.fingerprint) THEN
        RAISE EXCEPTION 'a validated payment instruction is frozen; invalidate the approval to change it'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
           (OLD.state = 'DRAFT'                AND NEW.state IN ('VALIDATED', 'CANCELLED', 'SUPERSEDED'))
        OR (OLD.state = 'VALIDATED'            AND NEW.state IN ('APPROVED', 'DRAFT', 'CANCELLED', 'SUPERSEDED'))
        OR (OLD.state = 'APPROVED'             AND NEW.state IN ('SUBMITTED_TO_BANKING', 'DRAFT', 'CANCELLED', 'SUPERSEDED'))
        OR (OLD.state = 'SUBMITTED_TO_BANKING' AND NEW.state = 'HANDED_OFF')
        OR (OLD.state = 'SUBMITTED_TO_BANKING' AND NEW.state = 'APPROVED' AND OLD.attempt_id IS NULL)) THEN
        RAISE EXCEPTION 'illegal payment instruction transition % -> %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
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
    -- Once handed to Banking the original attempt is resolved with Banking, never rewritten here.
    IF OLD.state IN ('SUBMITTED_TO_BANKING', 'HANDED_OFF') AND OLD.attempt_id IS NOT NULL AND NEW.attempt_id IS DISTINCT FROM OLD.attempt_id THEN
        RAISE EXCEPTION 'the Banking attempt of an instruction never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_pi_update_guard_trg ON wfp_payment_instructions;
CREATE TRIGGER wfp_pi_update_guard_trg BEFORE UPDATE OR DELETE ON wfp_payment_instructions FOR EACH ROW EXECUTE FUNCTION wfp_pi_update_guard();

-- An approval is made by someone other than the preparer and the principal who received the
-- payroll result, against the instruction's current fingerprint, while it is VALIDATED.
CREATE OR REPLACE FUNCTION wfp_pa_guard() RETURNS trigger AS $$
DECLARE i RECORD; received TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment approvals are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF NEW.instruction_id IS DISTINCT FROM OLD.instruction_id OR NEW.approver IS DISTINCT FROM OLD.approver
           OR NEW.fingerprint IS DISTINCT FROM OLD.fingerprint OR OLD.invalidated_at IS NOT NULL THEN
            RAISE EXCEPTION 'an approval can only be invalidated' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    SELECT state, fingerprint, created_by, result_id INTO i FROM wfp_payment_instructions WHERE instruction_id = NEW.instruction_id;
    IF i.state IS DISTINCT FROM 'VALIDATED' THEN
        RAISE EXCEPTION 'only a VALIDATED instruction can be approved (it is %)', i.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.fingerprint IS DISTINCT FROM i.fingerprint THEN
        RAISE EXCEPTION 'approval fingerprint does not match the instruction' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT received_by INTO received FROM wfp_payroll_results WHERE result_id = i.result_id;
    IF NEW.approver = i.created_by OR NEW.approver = received THEN
        RAISE EXCEPTION 'separation of duties: the preparer and the principal who received the result cannot approve its payment'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS wfp_pa_guard_trg ON wfp_payment_approvals;
CREATE TRIGGER wfp_pa_guard_trg BEFORE INSERT OR UPDATE OR DELETE ON wfp_payment_approvals FOR EACH ROW EXECUTE FUNCTION wfp_pa_guard();

-- ── Row-level security ───────────────────────────────────────────────────────
ALTER TABLE wfp_payment_instructions ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payment_instructions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payment_instructions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE wfp_payment_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE wfp_payment_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON wfp_payment_approvals FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- One approved instruction settles at most once.
CREATE UNIQUE INDEX IF NOT EXISTS uq_wfp_settle_instruction ON wfp_liability_settlements (instruction_id) WHERE instruction_id IS NOT NULL;
