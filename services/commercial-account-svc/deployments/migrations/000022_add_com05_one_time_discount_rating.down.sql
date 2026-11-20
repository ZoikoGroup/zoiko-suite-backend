-- 000022_add_com05_one_time_discount_rating.down.sql
DROP INDEX IF EXISTS idx_invoice_lines_subscription_one_time;

ALTER TABLE invoice_lines DROP CONSTRAINT IF EXISTS invoice_lines_discount_has_application;
ALTER TABLE invoice_lines DROP COLUMN IF EXISTS discount_application_id;
ALTER TABLE invoice_lines DROP CONSTRAINT IF EXISTS invoice_lines_kind_check;
ALTER TABLE invoice_lines ADD CONSTRAINT invoice_lines_kind_check CHECK (kind IN ('RECURRING', 'USAGE'));

ALTER TABLE invoice_candidate_lines DROP CONSTRAINT IF EXISTS invoice_candidate_lines_discount_has_application;
ALTER TABLE invoice_candidate_lines DROP COLUMN IF EXISTS discount_application_id;
ALTER TABLE invoice_candidate_lines DROP CONSTRAINT IF EXISTS invoice_candidate_lines_kind_check;
ALTER TABLE invoice_candidate_lines ADD CONSTRAINT invoice_candidate_lines_kind_check CHECK (kind IN ('RECURRING', 'USAGE'));
