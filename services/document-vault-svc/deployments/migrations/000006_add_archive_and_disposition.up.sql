-- BIZ-01 Wave 3: MoveToArchive + RequestDisposition.
--
-- ARCHIVED joins the status enum (ends a document's active life, same
-- posture as SUPERSEDED in migration 000005). PURGE_PENDING already
-- existed in the original schema (000001) but nothing ever set it —
-- RequestDisposition is that missing command. Per the doc's own
-- dependency line for BIZ-01 (DATA-GOV owns actual retention/disposition
-- policy), RequestDisposition only RECORDS the request and hands off; it
-- does not itself purge anything, and this migration adds no purge
-- mechanism — inventing one would fabricate a policy engine this service
-- was never given.

ALTER TABLE documents
    DROP CONSTRAINT documents_status_check,
    ADD CONSTRAINT documents_status_check
        CHECK (status IN ('ACTIVE', 'RETAINED', 'PURGE_PENDING', 'SUPERSEDED', 'ARCHIVED')),
    ADD COLUMN archived_at                        TIMESTAMPTZ,
    ADD COLUMN archived_by_principal_id            VARCHAR(255),
    ADD COLUMN archive_reason                      TEXT,
    ADD COLUMN disposition_requested_at            TIMESTAMPTZ,
    ADD COLUMN disposition_requested_by_principal_id VARCHAR(255),
    ADD COLUMN disposition_reason                  TEXT,
    ADD CONSTRAINT documents_archive_fields_consistent
        CHECK (
            (archived_at IS NULL AND archived_by_principal_id IS NULL)
            OR (archived_at IS NOT NULL AND archived_by_principal_id IS NOT NULL)
        ),
    ADD CONSTRAINT documents_disposition_fields_consistent
        CHECK (
            (disposition_requested_at IS NULL AND disposition_requested_by_principal_id IS NULL)
            OR (disposition_requested_at IS NOT NULL AND disposition_requested_by_principal_id IS NOT NULL)
        );

-- Same immutability posture as migration 000005's declaration/supersede
-- guard: once archived, or once a disposition request exists, neither can
-- be silently reversed by a raw UPDATE.
CREATE OR REPLACE FUNCTION reject_document_archive_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.archived_at IS NOT NULL AND (
        NEW.archived_at IS DISTINCT FROM OLD.archived_at
        OR NEW.archived_by_principal_id IS DISTINCT FROM OLD.archived_by_principal_id
    ) THEN
        RAISE EXCEPTION 'document % was archived at %; archival cannot be changed or reversed', OLD.document_id, OLD.archived_at;
    END IF;
    IF OLD.disposition_requested_at IS NOT NULL AND (
        NEW.disposition_requested_at IS DISTINCT FROM OLD.disposition_requested_at
        OR NEW.disposition_requested_by_principal_id IS DISTINCT FROM OLD.disposition_requested_by_principal_id
    ) THEN
        RAISE EXCEPTION 'document % already has a disposition request as of %; it cannot be changed or reversed here', OLD.document_id, OLD.disposition_requested_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_document_archive_mutation
    BEFORE UPDATE ON documents
    FOR EACH ROW EXECUTE FUNCTION reject_document_archive_mutation();
