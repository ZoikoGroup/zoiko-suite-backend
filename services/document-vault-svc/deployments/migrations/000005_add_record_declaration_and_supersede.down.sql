DROP TRIGGER IF EXISTS trg_reject_document_declaration_mutation ON documents;
DROP FUNCTION IF EXISTS reject_document_declaration_mutation();

ALTER TABLE documents
    DROP CONSTRAINT IF EXISTS documents_declaration_fields_consistent,
    DROP CONSTRAINT IF EXISTS documents_declared_version_positive,
    DROP COLUMN IF EXISTS superseded_by_document_id,
    DROP COLUMN IF EXISTS declared_by_principal_id,
    DROP COLUMN IF EXISTS declared_at,
    DROP COLUMN IF EXISTS declared_version,
    DROP CONSTRAINT documents_status_check,
    ADD CONSTRAINT documents_status_check
        CHECK (status IN ('ACTIVE', 'RETAINED', 'PURGE_PENDING'));
