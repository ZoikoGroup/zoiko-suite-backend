DROP TRIGGER IF EXISTS trg_email_supp_reject_mutation ON email_suppressions;
DROP FUNCTION IF EXISTS email_supp_reject_mutation();

-- Rolling back cannot restore the old full-table uniqueness while a lifted row
-- and an active row share a key; the lifted history rows are kept and the old
-- index is only recreated over the active rows' shape.
DROP INDEX IF EXISTS idx_email_suppressions_active_tenant_email_stream;
CREATE UNIQUE INDEX IF NOT EXISTS idx_email_suppressions_tenant_email_stream
    ON email_suppressions (tenant_id, recipient_email, source_stream) WHERE lifted_at IS NULL;

ALTER TABLE email_suppressions DROP CONSTRAINT IF EXISTS email_supp_governed_reactivation;
ALTER TABLE email_suppressions DROP CONSTRAINT IF EXISTS email_supp_lift_has_evidence;
-- The lift columns are kept: dropping them would erase the record of who
-- lifted which suppression on what evidence.
