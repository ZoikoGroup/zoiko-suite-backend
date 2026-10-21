DROP TABLE IF EXISTS event_outbox;

DROP TRIGGER IF EXISTS consent_invalidations_append_only ON consent_invalidations;
DROP TABLE IF EXISTS consent_invalidations;

DROP TRIGGER IF EXISTS consent_requests_append_only ON consent_requests;
DROP TABLE IF EXISTS consent_requests;

ALTER TABLE withdrawal_receipts
    DROP COLUMN IF EXISTS propagation_correlation_id;

ALTER TABLE consent_receipts
    DROP COLUMN IF EXISTS receipt_integrity_hash,
    DROP COLUMN IF EXISTS age_band,
    DROP COLUMN IF EXISTS is_offline_import,
    DROP COLUMN IF EXISTS import_source,
    DROP COLUMN IF EXISTS import_capture_method,
    DROP COLUMN IF EXISTS import_evidence_ref,
    DROP COLUMN IF EXISTS imported_at,
    DROP COLUMN IF EXISTS expires_at;

ALTER TABLE notice_versions
    DROP COLUMN IF EXISTS published_by_principal_id,
    DROP COLUMN IF EXISTS published_at,
    DROP COLUMN IF EXISTS effective_to;
