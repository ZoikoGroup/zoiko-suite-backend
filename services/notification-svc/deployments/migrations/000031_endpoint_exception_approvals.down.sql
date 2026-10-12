DELETE FROM ncd_approvals WHERE kind = 'ENDPOINT_EXCEPTION';
ALTER TABLE ncd_approvals DROP CONSTRAINT IF EXISTS ncd_approvals_kind_check;
ALTER TABLE ncd_approvals ADD CONSTRAINT ncd_approvals_kind_check
    CHECK (kind IN ('RESEND', 'MANUAL_EVIDENCE', 'SUPPRESSION_LIFT', 'BULK_SEND'));
