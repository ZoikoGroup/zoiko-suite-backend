-- BIZ-01 Wave 2: DeclareRecord + SupersedeDocument, and the doc's own
-- lifecycle line "Draft/Active -> Superseded/Declared Record ->
-- Archived/Disposed; record declaration tracked separately."
--
-- Read literally: declaration is a dimension ORTHOGONAL to the primary
-- status (a document can be ACTIVE and declared, or ACTIVE and not
-- declared), not a status value itself. SUPERSEDED, by contrast, ends a
-- document's active life the same way it does for AUD-06's evidence
-- (migration 000003's evidence_versions.superseded_by_evidence_version_id
-- pattern, reused here verbatim) — so it joins the status enum.
-- ARCHIVED/DISPOSED are deliberately NOT added here; they belong to
-- Wave 3's MoveToArchive/RequestDisposition.

ALTER TABLE documents
    DROP CONSTRAINT documents_status_check,
    ADD CONSTRAINT documents_status_check
        CHECK (status IN ('ACTIVE', 'RETAINED', 'PURGE_PENDING', 'SUPERSEDED')),
    ADD COLUMN declared_version         INT,
    ADD COLUMN declared_at              TIMESTAMPTZ,
    ADD COLUMN declared_by_principal_id VARCHAR(255),
    -- Forward link only — mirrors evidence_versions.superseded_by_evidence_version_id.
    -- The OLD document points at the NEW one; the new document's own row
    -- is never rewritten to "belong to" the old one.
    ADD COLUMN superseded_by_document_id UUID REFERENCES documents(document_id),
    -- declared_version must reference a version that actually exists —
    -- enforced at the application layer against document_versions, since a
    -- cross-table CHECK isn't expressible here; this CHECK only catches the
    -- cheap, always-true case of a non-positive version number.
    ADD CONSTRAINT documents_declared_version_positive
        CHECK (declared_version IS NULL OR declared_version > 0),
    -- All three declaration columns are set together or not at all.
    ADD CONSTRAINT documents_declaration_fields_consistent
        CHECK (
            (declared_version IS NULL AND declared_at IS NULL AND declared_by_principal_id IS NULL)
            OR (declared_version IS NOT NULL AND declared_at IS NOT NULL AND declared_by_principal_id IS NOT NULL)
        );

-- Once a document is declared a record, or once it has been superseded,
-- neither can be silently reversed by a raw UPDATE — only a genuinely new
-- write from NULL is permitted, same posture as
-- reject_evidence_version_mutation (migration 000003).
CREATE OR REPLACE FUNCTION reject_document_declaration_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.declared_at IS NOT NULL AND (
        NEW.declared_at IS DISTINCT FROM OLD.declared_at
        OR NEW.declared_version IS DISTINCT FROM OLD.declared_version
        OR NEW.declared_by_principal_id IS DISTINCT FROM OLD.declared_by_principal_id
    ) THEN
        RAISE EXCEPTION 'document % is already a declared record as of version %; declaration cannot be changed or reversed', OLD.document_id, OLD.declared_version;
    END IF;
    IF OLD.superseded_by_document_id IS NOT NULL
        AND NEW.superseded_by_document_id IS DISTINCT FROM OLD.superseded_by_document_id THEN
        RAISE EXCEPTION 'document % has already been superseded by %; this cannot be changed', OLD.document_id, OLD.superseded_by_document_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_document_declaration_mutation
    BEFORE UPDATE ON documents
    FOR EACH ROW EXECUTE FUNCTION reject_document_declaration_mutation();
