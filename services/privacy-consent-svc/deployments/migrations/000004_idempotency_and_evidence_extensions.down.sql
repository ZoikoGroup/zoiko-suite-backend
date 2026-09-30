-- 000004_idempotency_and_evidence_extensions.down.sql
DROP TABLE IF EXISTS consent_idempotency_keys;

ALTER TABLE presentation_receipts
    DROP COLUMN IF EXISTS session_ref,
    DROP COLUMN IF EXISTS template_version,
    DROP COLUMN IF EXISTS delivery_evidence;

ALTER TABLE consent_receipts
    DROP COLUMN IF EXISTS is_proxy,
    DROP COLUMN IF EXISTS representative_subject_ref,
    DROP COLUMN IF EXISTS representative_authority_ref,
    DROP COLUMN IF EXISTS representative_evidence,
    DROP COLUMN IF EXISTS affirmative_action_type,
    DROP COLUMN IF EXISTS affirmative_evidence;
