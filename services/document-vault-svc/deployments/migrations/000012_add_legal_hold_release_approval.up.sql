-- ZS-SVC-S-001 §5.5's legal-hold field table names two maker-checker
-- columns this service never modeled: "issued_by / approved_by:
-- Authorized roles; maker-checker per policy" and "release_approved_by:
-- Separate release authority; self-release restrictions apply." Migration
-- 000010 modeled issued_by/activated_by/released_by but never added a
-- release approver column or its self-release check — unlike
-- retention_rule_versions in that same migration, which DOES enforce
-- "approved_by_principal_id <> created_by_principal_id" at the database
-- level. This brings legal_holds' release path to the same standard.
ALTER TABLE legal_holds
    ADD COLUMN release_approved_by_principal_id VARCHAR(255);

ALTER TABLE legal_holds
    ADD CONSTRAINT legal_holds_release_approval_paired
    CHECK ((release_approved_by_principal_id IS NULL) = (released_at IS NULL));

-- The self-release restriction itself: the principal who approved the
-- release cannot be the same principal who executed it. Same shape as
-- retention_rule_versions' own maker-checker CHECK in migration 000010.
ALTER TABLE legal_holds
    ADD CONSTRAINT legal_holds_no_self_release
    CHECK (release_approved_by_principal_id IS NULL OR release_approved_by_principal_id <> released_by_principal_id);
