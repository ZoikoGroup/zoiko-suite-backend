-- 000002_immutability.up.sql
-- Enforces PRV-I06 (published purpose versions are immutable) and the
-- structural half of PRV-I20/I21 (no destructive rewriting of a governed
-- historical processing-activity version) at the database layer — not
-- just in application code, same doctrine as evidence-manifest-svc's
-- reject_mutation() trigger. This matters even though the runtime
-- connects as a superuser: GRANT/REVOKE cannot bind a superuser, so only
-- a trigger actually stops it.
--
-- purpose_versions: once a version is PUBLISHED, ZERO further mutation of
-- any kind is legal — PRV-I06's wording is unconditional ("published...
-- versions are immutable"), so there is no separate "structural fields
-- only" carve-out the way processing_activity_versions has. Amending a
-- published purpose means creating a new version via
-- supersedes_version_id, never touching this row again.
CREATE OR REPLACE FUNCTION reject_published_purpose_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.version_status = 'PUBLISHED' THEN
        RAISE EXCEPTION 'purpose_versions is immutable once PUBLISHED (row %)', OLD.purpose_version_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER purpose_versions_immutable_once_published
    BEFORE UPDATE ON purpose_versions
    FOR EACH ROW EXECUTE FUNCTION reject_published_purpose_mutation();

-- processing_activity_versions is different: version_status legitimately
-- progresses many times after DRAFT (DRAFT -> VALIDATED -> SUBMITTED ->
-- APPROVED -> ACTIVE -> SUSPENDED -> ... -> RETIRED, or -> REJECTED), so
-- blocking all mutation the moment it leaves DRAFT would make the
-- lifecycle itself impossible. What PRV-I20 actually protects is the
-- CONTENT — what is processed, why, under what role — not the status.
-- Once a version leaves DRAFT, its content fields lock; only
-- version_status and the fields the lifecycle actions themselves set
-- (validation_findings, rejection_reason, effective_from) may still
-- change. Amending content after DRAFT means creating a new version via
-- supersedes_version_id.
CREATE OR REPLACE FUNCTION reject_activity_content_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.version_status <> 'DRAFT' THEN
        IF NEW.activity_id IS DISTINCT FROM OLD.activity_id
           OR NEW.privacy_role IS DISTINCT FROM OLD.privacy_role
           OR NEW.owner IS DISTINCT FROM OLD.owner
           OR NEW.purpose_ids IS DISTINCT FROM OLD.purpose_ids
           OR NEW.subject_classes IS DISTINCT FROM OLD.subject_classes
           OR NEW.data_categories IS DISTINCT FROM OLD.data_categories
           OR NEW.sources IS DISTINCT FROM OLD.sources
           OR NEW.recipients IS DISTINCT FROM OLD.recipients
           OR NEW.jurisdictions IS DISTINCT FROM OLD.jurisdictions
           OR NEW.retention_rule_refs IS DISTINCT FROM OLD.retention_rule_refs
           OR NEW.transfer_refs IS DISTINCT FROM OLD.transfer_refs
           OR NEW.supersedes_version_id IS DISTINCT FROM OLD.supersedes_version_id
           OR NEW.created_at IS DISTINCT FROM OLD.created_at
           OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        THEN
            RAISE EXCEPTION 'processing_activity_versions content is immutable once it leaves DRAFT: only version_status/validation_findings/rejection_reason/effective_from may change (row %)', OLD.activity_version_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER activity_versions_content_immutable
    BEFORE UPDATE ON processing_activity_versions
    FOR EACH ROW EXECUTE FUNCTION reject_activity_content_mutation();
