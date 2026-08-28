DROP TRIGGER IF EXISTS preference_assertions_append_only ON preference_assertions;
DROP TRIGGER IF EXISTS withdrawal_receipts_append_only ON withdrawal_receipts;
DROP TRIGGER IF EXISTS consent_receipts_append_only ON consent_receipts;
DROP TRIGGER IF EXISTS presentation_receipts_append_only ON presentation_receipts;
DROP FUNCTION IF EXISTS reject_evidence_mutation();

DROP TRIGGER IF EXISTS notice_versions_content_immutable ON notice_versions;
DROP FUNCTION IF EXISTS reject_notice_content_mutation();
