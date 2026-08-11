-- 000003_add_explicit_scope_type.up.sql
--
-- docs/original_doc/zoiko_suite_doc7.txt §F1: "'GLOBAL' is an explicit
-- scope, not a default." Until now this service inferred "global" purely
-- from tenant_id/legal_entity_id being NULL — that satisfies the actual
-- query logic (COALESCE-based dedup, "applicable version" lookup) but never
-- surfaces the scope as a named, explicit value the way the spec describes.
--
-- scope_type makes the intent explicit without discarding the working
-- nullable-column mechanism: the CHECK constraint below ties scope_type to
-- the existing tenant_id/legal_entity_id nullness so the two can never
-- drift apart, and every existing row is backfilled deterministically from
-- its current nullness pattern.

ALTER TABLE policy_versions
    ADD COLUMN scope_type VARCHAR(16) NOT NULL DEFAULT 'GLOBAL';

UPDATE policy_versions
SET scope_type = CASE
    WHEN tenant_id IS NULL THEN 'GLOBAL'
    WHEN legal_entity_id IS NULL THEN 'TENANT'
    ELSE 'LEGAL_ENTITY'
END;

ALTER TABLE policy_versions
    ALTER COLUMN scope_type DROP DEFAULT;

ALTER TABLE policy_versions
    ADD CONSTRAINT chk_policy_versions_scope_type CHECK (
        (scope_type = 'GLOBAL' AND tenant_id IS NULL AND legal_entity_id IS NULL)
        OR (scope_type = 'TENANT' AND tenant_id IS NOT NULL AND legal_entity_id IS NULL)
        OR (scope_type = 'LEGAL_ENTITY' AND tenant_id IS NOT NULL AND legal_entity_id IS NOT NULL)
    );
