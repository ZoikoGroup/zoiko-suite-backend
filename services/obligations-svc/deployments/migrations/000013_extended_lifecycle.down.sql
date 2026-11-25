-- 000013_extended_lifecycle.down.sql
BEGIN;

ALTER TABLE obligations DROP CONSTRAINT IF EXISTS obligations_status_known;
ALTER TABLE obligations
    ADD CONSTRAINT obligations_status_known
    CHECK (obligation_status IN ('OPEN', 'IN_PROGRESS', 'OVERDUE', 'CLOSED')) NOT VALID;

DROP INDEX IF EXISTS idx_filing_requirements_period;
DROP INDEX IF EXISTS idx_filing_requirements_jurisdiction;

ALTER TABLE filing_requirements DROP CONSTRAINT IF EXISTS chk_filing_requirements_status;
ALTER TABLE filing_requirements ADD CONSTRAINT chk_filing_requirements_status
    CHECK (filing_status IN ('PENDING', 'IN_PREPARATION', 'SUBMITTED', 'ACCEPTED', 'REJECTED', 'WITHDRAWN'))
    NOT VALID;

ALTER TABLE filing_requirements DROP CONSTRAINT IF EXISTS filing_requirements_status_known;
ALTER TABLE filing_requirements
    ADD CONSTRAINT filing_requirements_status_known
    CHECK (filing_status IN ('PENDING', 'SUBMITTED', 'ACCEPTED', 'REJECTED')) NOT VALID;

ALTER TABLE filing_requirements DROP COLUMN IF EXISTS notes;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS rejection_reason;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS confirmed_at;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS confirmation_reference;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS submitted_by;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS submitted_at;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS submission_reference;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS due_date;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS period_key;
ALTER TABLE filing_requirements DROP COLUMN IF EXISTS jurisdiction_id;

DROP INDEX IF EXISTS idx_obligations_extended_due;
DROP INDEX IF EXISTS idx_obligations_candidates;

ALTER TABLE obligations DROP COLUMN IF EXISTS effective_to;
ALTER TABLE obligations DROP COLUMN IF EXISTS effective_from;
ALTER TABLE obligations DROP COLUMN IF EXISTS superseded_by_obligation_id;
ALTER TABLE obligations DROP COLUMN IF EXISTS dispute_reason;
ALTER TABLE obligations DROP COLUMN IF EXISTS disputed_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS disputed_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS breach_note;
ALTER TABLE obligations DROP COLUMN IF EXISTS breached_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS breached_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS not_applicable_reason;
ALTER TABLE obligations DROP COLUMN IF EXISTS not_applicable_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS not_applicable_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS waiver_reason_code;
ALTER TABLE obligations DROP COLUMN IF EXISTS waiver_authority_reference;
ALTER TABLE obligations DROP COLUMN IF EXISTS waived_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS waived_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS fulfillment_evidence_ref;
ALTER TABLE obligations DROP COLUMN IF EXISTS fulfilled_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS fulfilled_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS in_progress_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS in_progress_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS due_marked_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS due_marked_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS due_soon_marked_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS scheduled_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS scheduled_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS calculation_method;
ALTER TABLE obligations DROP COLUMN IF EXISTS trigger_description;
ALTER TABLE obligations DROP COLUMN IF EXISTS source_clause_id;
ALTER TABLE obligations DROP COLUMN IF EXISTS validated_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS validated_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS extracted_by_ai;
ALTER TABLE obligations DROP COLUMN IF EXISTS clock_class;
ALTER TABLE obligations DROP CONSTRAINT IF EXISTS chk_obligations_lifecycle_model;
ALTER TABLE obligations DROP COLUMN IF EXISTS lifecycle_model;

COMMIT;
