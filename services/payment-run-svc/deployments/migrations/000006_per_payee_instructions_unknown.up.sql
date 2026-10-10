-- ZS-SVC-D-001 AP-11 hardening.
--
-- 1. One instruction per payee. An AP-10 authorization covers a whole AP-09
--    proposal, which may batch payables for several suppliers. Previously
--    one instruction was created per authorization and the proposal's full
--    net amount was sent to the first payee only. An instruction is now
--    unique per (authorization, payee), and the payables it settles are
--    recorded in run_instruction_payables.
-- 2. External UNKNOWN is distinct from FAILED (invariant #18). A Banking
--    hand-off with no authoritative answer leaves the instruction in
--    PENDING_UNKNOWN, and the run in PENDING_UNKNOWN, instead of EXCEPTION.
-- 3. Every instruction status change carries a reason (spec §16
--    reason_code), stored both on the reconciliation event and the
--    instruction itself.

DROP INDEX IF EXISTS uq_run_instructions_authorization;
CREATE UNIQUE INDEX uq_run_instructions_authorization_payee
    ON run_instructions (authorization_id, payee_ref);

ALTER TABLE run_instructions DROP CONSTRAINT IF EXISTS run_instructions_status_check;
ALTER TABLE run_instructions ADD CONSTRAINT run_instructions_status_check
    CHECK (status IN ('PENDING', 'PENDING_UNKNOWN', 'ACCEPTED', 'REJECTED', 'SETTLED', 'EXCEPTION'));

ALTER TABLE run_instructions ADD COLUMN status_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE payment_runs DROP CONSTRAINT IF EXISTS payment_runs_status_check;
ALTER TABLE payment_runs ADD CONSTRAINT payment_runs_status_check
    CHECK (status IN ('DRAFT', 'VALIDATED', 'LOCKED', 'SUBMITTED', 'PENDING_UNKNOWN', 'ACCEPTED',
                      'REJECTED', 'PARTIALLY_ACCEPTED', 'SETTLED', 'COMPLETED',
                      'EXCEPTION', 'CANCELLED'));

ALTER TABLE instruction_reconciliation_events
    DROP CONSTRAINT IF EXISTS instruction_reconciliation_events_external_status_check;
ALTER TABLE instruction_reconciliation_events
    ADD CONSTRAINT instruction_reconciliation_events_external_status_check
    CHECK (external_status IN ('PENDING', 'PENDING_UNKNOWN', 'ACCEPTED', 'REJECTED', 'SETTLED', 'EXCEPTION'));
ALTER TABLE instruction_reconciliation_events ADD COLUMN reason TEXT NOT NULL DEFAULT '';

-- The AP-08 payables an instruction settles, frozen at CreateRun from the
-- authorized AP-09 proposal items. payable_applied_at records that AP-08
-- has accepted the settlement application for this payable, so a failed
-- AP-08 call is retried on the next poll instead of being lost.
CREATE TABLE run_instruction_payables (
    instruction_id      UUID NOT NULL REFERENCES run_instructions (instruction_id),
    tenant_id           UUID NULL,
    payable_source      TEXT NOT NULL CHECK (payable_source IN ('AP_INVOICE', 'EXPENSE_CLAIM')),
    source_reference    TEXT NOT NULL,
    gross_amount        NUMERIC(18, 2) NOT NULL,
    withholding_amount  NUMERIC(18, 2) NOT NULL DEFAULT 0,
    net_amount          NUMERIC(18, 2) NOT NULL CHECK (net_amount > 0),
    payable_applied_at  TIMESTAMPTZ NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (instruction_id, payable_source, source_reference)
);

CREATE INDEX idx_run_instruction_payables_unapplied
    ON run_instruction_payables (instruction_id) WHERE payable_applied_at IS NULL;

-- Everything but payable_applied_at is fixed at insert, and that column is
-- set once.
CREATE OR REPLACE FUNCTION reject_run_instruction_payable_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'run_instruction_payables rows are never deleted';
    END IF;

    IF NEW.instruction_id IS DISTINCT FROM OLD.instruction_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.payable_source IS DISTINCT FROM OLD.payable_source
        OR NEW.source_reference IS DISTINCT FROM OLD.source_reference
        OR NEW.gross_amount IS DISTINCT FROM OLD.gross_amount
        OR NEW.withholding_amount IS DISTINCT FROM OLD.withholding_amount
        OR NEW.net_amount IS DISTINCT FROM OLD.net_amount
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'run instruction payable may only have payable_applied_at set, nothing else';
    END IF;

    IF OLD.payable_applied_at IS NOT NULL AND NEW.payable_applied_at IS DISTINCT FROM OLD.payable_applied_at THEN
        RAISE EXCEPTION 'run instruction payable payable_applied_at is already set and cannot change';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_run_instruction_payable_mutation
    BEFORE UPDATE OR DELETE ON run_instruction_payables
    FOR EACH ROW EXECUTE FUNCTION reject_run_instruction_payable_mutation();

ALTER TABLE run_instruction_payables ENABLE ROW LEVEL SECURITY;
ALTER TABLE run_instruction_payables FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON run_instruction_payables
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
