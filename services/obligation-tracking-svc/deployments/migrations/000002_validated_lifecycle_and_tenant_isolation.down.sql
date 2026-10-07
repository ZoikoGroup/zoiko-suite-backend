-- +migrate Down
BEGIN;

ALTER TABLE obligations NO FORCE ROW LEVEL SECURITY;

ALTER TABLE obligations DROP COLUMN IF EXISTS superseded_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS dispute_reason;
ALTER TABLE obligations DROP COLUMN IF EXISTS disputed_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS disputed_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS breach_note;
ALTER TABLE obligations DROP COLUMN IF EXISTS breached_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS breached_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS waiver_authority_reference;
ALTER TABLE obligations DROP COLUMN IF EXISTS waived_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS waived_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS satisfied_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS satisfied_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS in_progress_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS in_progress_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS due_marked_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS due_marked_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS scheduled_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS scheduled_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS calculation_method;
ALTER TABLE obligations DROP COLUMN IF EXISTS trigger_description;
ALTER TABLE obligations DROP COLUMN IF EXISTS source_clause_id;
ALTER TABLE obligations DROP COLUMN IF EXISTS validated_by;
ALTER TABLE obligations DROP COLUMN IF EXISTS validated_at;
ALTER TABLE obligations DROP COLUMN IF EXISTS extracted_by_ai;

ALTER TABLE obligations DROP CONSTRAINT IF EXISTS obligations_status_known;
ALTER TABLE obligations
    ADD CONSTRAINT obligations_status_known
    CHECK (status IN ('PENDING','IN_PROGRESS','FULFILLED','BREACHED','WAIVED')) NOT VALID;

COMMIT;
