-- 000002_immutability.up.sql
-- Enforces the immutability doctrine described in internal/domain's
-- package doc comment, at the database layer — same reasoning as
-- privacy-purpose-registry-svc's equivalent migration: GRANT/REVOKE
-- cannot bind the superuser this runtime connects as, so only a trigger
-- actually stops it.
--
-- notice_versions is like PRV-01's processing_activity_versions: status
-- legitimately progresses (DRAFT -> APPROVED -> PUBLISHED -> SUPERSEDED/
-- WITHDRAWN), so only CONTENT is locked once a version leaves DRAFT —
-- locale/audience/content_hash. Amending content means creating a new
-- version via supersedes_version_id.
CREATE OR REPLACE FUNCTION reject_notice_content_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.version_status <> 'DRAFT' THEN
        IF NEW.notice_id IS DISTINCT FROM OLD.notice_id
           OR NEW.locale IS DISTINCT FROM OLD.locale
           OR NEW.audience IS DISTINCT FROM OLD.audience
           OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
           OR NEW.supersedes_version_id IS DISTINCT FROM OLD.supersedes_version_id
           OR NEW.created_at IS DISTINCT FROM OLD.created_at
           OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        THEN
            RAISE EXCEPTION 'notice_versions content is immutable once it leaves DRAFT (row %)', OLD.notice_version_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER notice_versions_content_immutable
    BEFORE UPDATE ON notice_versions
    FOR EACH ROW EXECUTE FUNCTION reject_notice_content_mutation();

-- The four evidence tables are pure append-only logs — every row, once
-- written, is permanent. There is no legitimate reason for ANY of these
-- four to ever be updated or deleted; a single generic trigger function
-- covers all of them.
CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only evidence: % is never permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER presentation_receipts_append_only
    BEFORE UPDATE OR DELETE ON presentation_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER consent_receipts_append_only
    BEFORE UPDATE OR DELETE ON consent_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER withdrawal_receipts_append_only
    BEFORE UPDATE OR DELETE ON withdrawal_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

CREATE TRIGGER preference_assertions_append_only
    BEFORE UPDATE OR DELETE ON preference_assertions
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();
