-- +migrate Down
BEGIN;

ALTER TABLE contract_versions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE contracts NO FORCE ROW LEVEL SECURITY;

ALTER TABLE contracts DROP COLUMN IF EXISTS renewed_by;
ALTER TABLE contracts DROP COLUMN IF EXISTS renewed_at;
ALTER TABLE contracts DROP COLUMN IF EXISTS amended_by;
ALTER TABLE contracts DROP COLUMN IF EXISTS amended_at;
ALTER TABLE contracts DROP COLUMN IF EXISTS effective_at;
ALTER TABLE contracts DROP COLUMN IF EXISTS executed_at;
ALTER TABLE contracts DROP COLUMN IF EXISTS signature_sent_at;
ALTER TABLE contracts DROP COLUMN IF EXISTS approved_by;
ALTER TABLE contracts DROP COLUMN IF EXISTS approved_at;
ALTER TABLE contracts DROP COLUMN IF EXISTS submitted_by;
ALTER TABLE contracts DROP COLUMN IF EXISTS submitted_at;

ALTER TABLE contracts DROP CONSTRAINT IF EXISTS contracts_signature_status_known;
ALTER TABLE contracts DROP COLUMN IF EXISTS signature_status;

ALTER TABLE contracts DROP CONSTRAINT IF EXISTS contracts_status_known;

COMMIT;
