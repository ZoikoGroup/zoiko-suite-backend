ALTER TABLE legal_holds DROP CONSTRAINT IF EXISTS legal_holds_no_self_release;
ALTER TABLE legal_holds DROP CONSTRAINT IF EXISTS legal_holds_release_approval_paired;
ALTER TABLE legal_holds DROP COLUMN IF EXISTS release_approved_by_principal_id;
