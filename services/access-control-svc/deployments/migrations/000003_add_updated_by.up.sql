-- access-control-svc: record who last changed a definition.
--
-- The catalogue could say WHAT changed (updated_at, and status for roles) but
-- never WHO changed it. created_by_principal_id has been on both tables since
-- 000001; the audit trail stopped at creation. This adds the mirror column for
-- updates so an operator reading a definition in this register can see the
-- author of its last change, not only of its creation.
--
-- Nullable on purpose. Rows written before this migration have no author for
-- their (nonexistent) update, and the column is only ever populated by the
-- update paths, which always have the verified caller. A CHECK-not-null here
-- would force a backfill of guesswork for rows that were never updated.

ALTER TABLE role_definitions
    ADD COLUMN updated_by_principal_id VARCHAR(255);

ALTER TABLE permission_bundle_defs
    ADD COLUMN updated_by_principal_id VARCHAR(255);