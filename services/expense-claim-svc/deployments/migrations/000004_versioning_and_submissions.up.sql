-- AP-07 hardening, part 1: optimistic version, distinct SUBMITTED/CLOSED
-- states, an enforced transition table, immutable submission snapshots and
-- voidable (never deletable) expense lines.

ALTER TABLE expense_claims
    ADD COLUMN version                 INT  NOT NULL DEFAULT 1,
    ADD COLUMN submitted_version       INT  NOT NULL DEFAULT 0,
    ADD COLUMN payable_id              TEXT NOT NULL DEFAULT '',
    ADD COLUMN payable_state           TEXT NOT NULL DEFAULT 'NONE'
               CHECK (payable_state IN ('NONE', 'PENDING', 'BLOCKED', 'CREATED')),
    ADD COLUMN payable_blocked_reason  TEXT NOT NULL DEFAULT '',
    ADD COLUMN payee_destination_id    TEXT NOT NULL DEFAULT '',
    ADD COLUMN closed_at               TIMESTAMPTZ NULL,
    ADD COLUMN close_reason            TEXT NOT NULL DEFAULT '';

ALTER TABLE expense_claims DROP CONSTRAINT expense_claims_status_check;
ALTER TABLE expense_claims ADD CONSTRAINT expense_claims_status_check
    CHECK (status IN ('DRAFT', 'SUBMITTED', 'PENDING_APPROVAL', 'APPROVED', 'REJECTED',
                      'RETURNED', 'REIMBURSABLE', 'CLOSED', 'CANCELLED'));

-- Lines are never deleted; a wrong line is voided (kept as evidence) and a
-- corrected one added. A voided line frees its receipt document.
ALTER TABLE expense_lines
    ADD COLUMN voided_at   TIMESTAMPTZ NULL,
    ADD COLUMN void_reason TEXT NOT NULL DEFAULT '';

DROP INDEX uq_expense_lines_receipt_document;
CREATE UNIQUE INDEX uq_expense_lines_receipt_document
    ON expense_lines (receipt_document_id)
    WHERE receipt_document_id IS NOT NULL AND voided_at IS NULL;

-- The state machine, enforced where a bug in a handler cannot bypass it.
-- Mirrors internal/domain's transition table (a store test asserts parity).
CREATE OR REPLACE FUNCTION expense_claim_transition_allowed(from_status TEXT, to_status TEXT) RETURNS BOOLEAN AS $$
    SELECT CASE from_status
        WHEN 'DRAFT'            THEN to_status IN ('SUBMITTED', 'CANCELLED')
        WHEN 'SUBMITTED'        THEN to_status IN ('PENDING_APPROVAL', 'CANCELLED')
        WHEN 'PENDING_APPROVAL' THEN to_status IN ('APPROVED', 'REJECTED', 'RETURNED', 'CANCELLED')
        WHEN 'RETURNED'         THEN to_status IN ('SUBMITTED', 'CANCELLED')
        WHEN 'APPROVED'         THEN to_status IN ('REIMBURSABLE')
        WHEN 'REIMBURSABLE'     THEN to_status IN ('CLOSED')
        ELSE FALSE
    END
$$ LANGUAGE sql IMMUTABLE;

CREATE OR REPLACE FUNCTION reject_expense_claim_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'expense_claims rows are never deleted';
    END IF;

    IF OLD.status IN ('REJECTED', 'CANCELLED', 'CLOSED') THEN
        RAISE EXCEPTION 'expense claim % is in terminal status % and cannot be modified', OLD.claim_id, OLD.status;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.claimant_principal_id IS DISTINCT FROM OLD.claimant_principal_id
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'expense claim % identity fields can never change', OLD.claim_id;
    END IF;

    IF OLD.approved_by_principal_id IS NOT NULL
        AND NEW.approved_by_principal_id IS DISTINCT FROM OLD.approved_by_principal_id THEN
        RAISE EXCEPTION 'expense claim % approval record can never change', OLD.claim_id;
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status
        AND NOT expense_claim_transition_allowed(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'expense claim % cannot move from % to %', OLD.claim_id, OLD.status, NEW.status
            USING ERRCODE = 'ZK002';
    END IF;

    IF NEW.version < OLD.version OR NEW.submitted_version < OLD.submitted_version THEN
        RAISE EXCEPTION 'expense claim % version counters only move forward', OLD.claim_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Lines: insert/amend only while the parent is DRAFT/RETURNED. Once written,
-- the evidence columns can never change; only the tax-determination result
-- (written by Submit) and a one-way void flag may.
CREATE OR REPLACE FUNCTION reject_expense_line_mutation() RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'expense_lines rows are never deleted';
    END IF;

    SELECT status INTO parent_status FROM expense_claims WHERE claim_id = NEW.claim_id;
    IF parent_status NOT IN ('DRAFT', 'RETURNED') THEN
        RAISE EXCEPTION 'claim % is in status % and can no longer accept new or amended expense lines', NEW.claim_id, parent_status
            USING ERRCODE = 'ZK001';
    END IF;

    IF TG_OP = 'UPDATE' THEN
        IF NEW.claim_id IS DISTINCT FROM OLD.claim_id
            OR NEW.merchant IS DISTINCT FROM OLD.merchant
            OR NEW.expense_date IS DISTINCT FROM OLD.expense_date
            OR NEW.amount IS DISTINCT FROM OLD.amount
            OR NEW.currency IS DISTINCT FROM OLD.currency
            OR NEW.receipt_document_id IS DISTINCT FROM OLD.receipt_document_id
            OR NEW.claim_tax_recovery IS DISTINCT FROM OLD.claim_tax_recovery
            OR NEW.jurisdiction IS DISTINCT FROM OLD.jurisdiction
            OR NEW.tax_category IS DISTINCT FROM OLD.tax_category
        THEN
            RAISE EXCEPTION 'expense line % evidence fields can never be edited; void it and add a corrected line', OLD.line_id
                USING ERRCODE = 'ZK003';
        END IF;
        IF OLD.voided_at IS NOT NULL AND NEW.voided_at IS DISTINCT FROM OLD.voided_at THEN
            RAISE EXCEPTION 'expense line % is voided and cannot be reinstated', OLD.line_id USING ERRCODE = 'ZK003';
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Immutable versioned submission snapshots: what the approver was shown.
-- A Return-for-correction never touches a prior version; resubmission adds
-- version N+1.
CREATE TABLE expense_claim_submissions (
    submission_id    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    claim_id         UUID NOT NULL REFERENCES expense_claims (claim_id),
    version_no       INT  NOT NULL CHECK (version_no >= 1),
    snapshot         JSONB NOT NULL,
    snapshot_hash    TEXT NOT NULL,
    submitted_by     TEXT NOT NULL,
    submitted_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_expense_claim_submission_version UNIQUE (claim_id, version_no)
);

CREATE TRIGGER trg_reject_expense_claim_submissions_mutation
    BEFORE UPDATE OR DELETE ON expense_claim_submissions
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

ALTER TABLE expense_claim_submissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE expense_claim_submissions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON expense_claim_submissions
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
