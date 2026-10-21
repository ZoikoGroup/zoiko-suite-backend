-- Reverse Wave 8. Instructions already RETURNED must be handled before running this.
DROP POLICY IF EXISTS tenant_isolation_policy ON wfp_payroll_liabilities;
CREATE POLICY tenant_isolation_policy ON wfp_payroll_liabilities FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
DROP POLICY IF EXISTS tenant_isolation_policy ON wfp_payment_instructions;
CREATE POLICY tenant_isolation_policy ON wfp_payment_instructions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
DROP TABLE IF EXISTS wfp_reconciliation_exceptions;
DROP TABLE IF EXISTS wfp_reconciliation_runs;
DROP TABLE IF EXISTS wfp_instruction_banking_observations;
DROP FUNCTION IF EXISTS wfp_re_guard();
DROP FUNCTION IF EXISTS wfp_rr_guard();
DROP FUNCTION IF EXISTS wfp_ibo_append_only();

ALTER TABLE wfp_payment_instructions DROP CONSTRAINT IF EXISTS wfp_pi_returned;
ALTER TABLE wfp_payment_instructions DROP CONSTRAINT IF EXISTS wfp_pi_handed_off;
ALTER TABLE wfp_payment_instructions ADD CONSTRAINT wfp_pi_handed_off CHECK ((state = 'HANDED_OFF') = (handed_off_at IS NOT NULL));
ALTER TABLE wfp_payment_instructions DROP CONSTRAINT IF EXISTS wfp_pi_state_check;
ALTER TABLE wfp_payment_instructions ADD CONSTRAINT wfp_pi_state_check CHECK (state IN
    ('DRAFT', 'VALIDATED', 'APPROVED', 'SUBMITTED_TO_BANKING', 'HANDED_OFF', 'CANCELLED', 'SUPERSEDED'));
ALTER TABLE wfp_payment_instructions DROP COLUMN IF EXISTS return_reason;
ALTER TABLE wfp_payment_instructions DROP COLUMN IF EXISTS returned_at;
ALTER TABLE wfp_payment_instructions DROP COLUMN IF EXISTS payment_id;
ALTER TABLE wfp_payment_instructions DROP COLUMN IF EXISTS banking_checked_at;
DROP INDEX IF EXISTS uq_wfp_pi_net_pay;
CREATE UNIQUE INDEX uq_wfp_pi_net_pay ON wfp_payment_instructions (tenant_id, result_id, worker_id)
    WHERE instruction_type = 'NET_PAY' AND state NOT IN ('CANCELLED', 'SUPERSEDED');

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
