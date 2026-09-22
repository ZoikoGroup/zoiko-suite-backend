DROP TRIGGER IF EXISTS trg_reject_document_archive_mutation ON documents;
DROP FUNCTION IF EXISTS reject_document_archive_mutation();

ALTER TABLE documents
    DROP CONSTRAINT IF EXISTS documents_disposition_fields_consistent,
    DROP CONSTRAINT IF EXISTS documents_archive_fields_consistent,
    DROP COLUMN IF EXISTS disposition_reason,
    DROP COLUMN IF EXISTS disposition_requested_by_principal_id,
    DROP COLUMN IF EXISTS disposition_requested_at,
    DROP COLUMN IF EXISTS archive_reason,
    DROP COLUMN IF EXISTS archived_by_principal_id,
    DROP COLUMN IF EXISTS archived_at,
    DROP CONSTRAINT documents_status_check,
    ADD CONSTRAINT documents_status_check
        CHECK (status IN ('ACTIVE', 'RETAINED', 'PURGE_PENDING', 'SUPERSEDED'));
