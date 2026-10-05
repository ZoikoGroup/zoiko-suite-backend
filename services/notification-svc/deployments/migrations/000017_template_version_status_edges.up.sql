-- 000017_template_version_status_edges.up.sql
-- ZS-SVC-Y-001 NCD-01 (section 4.3) and TC-02, Wave 2 slice 1 (audit finding F-08).
--
-- Migration 000005 froze a template version's CONTENT from the moment it is written
-- and froze terminal states, and a CHECK enforces maker-checker. What it did not do
-- is say which status changes are LEGAL. The store only ever makes the legal ones,
-- but that is the application, not the control: a direct UPDATE could take a DRAFT
-- version straight to PUBLISHED, skipping validation, review and approval, and the
-- immutability trigger would then protect that unreviewed wording forever.
--
-- The edges the store actually uses, and no others:
--
--   (insert)  -> DRAFT
--   DRAFT     -> REVIEW        needs validated_at
--   REVIEW    -> APPROVED      needs approved_by_principal_id and approved_at
--   APPROVED  -> PUBLISHED     needs published_at (and an approval)
--   PUBLISHED -> SUPERSEDED    needs superseded_by_version_id
--   PUBLISHED -> RETIRED       needs retired_at
--
-- The spec's reject path (REVIEW -> DRAFT) is deliberately NOT allowed yet: no
-- endpoint performs it, and a transition nothing uses is a hole, not a feature. It
-- is added together with the endpoint that needs it.
--
-- Rows already in the table are not re-checked; only the next change to a row is.

CREATE OR REPLACE FUNCTION guard_template_version_status() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'DRAFT' THEN
            RAISE EXCEPTION 'a template version is created as DRAFT, not %', NEW.status USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.status = OLD.status THEN
        RETURN NEW;
    END IF;

    IF NOT (
        (OLD.status = 'DRAFT'     AND NEW.status = 'REVIEW'     AND NEW.validated_at IS NOT NULL) OR
        (OLD.status = 'REVIEW'    AND NEW.status = 'APPROVED'   AND NEW.approved_at IS NOT NULL AND NEW.approved_by_principal_id IS NOT NULL) OR
        (OLD.status = 'APPROVED'  AND NEW.status = 'PUBLISHED'  AND NEW.published_at IS NOT NULL AND NEW.approved_at IS NOT NULL) OR
        (OLD.status = 'PUBLISHED' AND NEW.status = 'SUPERSEDED' AND NEW.superseded_by_version_id IS NOT NULL) OR
        (OLD.status = 'PUBLISHED' AND NEW.status = 'RETIRED'    AND NEW.retired_at IS NOT NULL)
    ) THEN
        RAISE EXCEPTION 'template version % cannot move from % to % (or is missing the evidence that move requires)',
            OLD.version_id, OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_template_version_status_edges ON template_versions;
CREATE TRIGGER trg_template_version_status_edges
    BEFORE INSERT OR UPDATE OF status ON template_versions
    FOR EACH ROW EXECUTE FUNCTION guard_template_version_status();
