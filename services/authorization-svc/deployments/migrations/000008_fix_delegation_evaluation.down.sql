-- Reverts 000008. The policy returns to 000006's form (no platform-scope
-- hatch), which re-breaks delegated access for every caller that sends no
-- tenant — that is what reverting this migration means, and it is why the
-- comment is here rather than only in the up file.
BEGIN;

DROP INDEX IF EXISTS idx_delegations_source_unique;
ALTER TABLE delegated_authorities DROP COLUMN IF EXISTS source_delegation_id;
ALTER TABLE delegated_authorities DROP COLUMN IF EXISTS source_service;
ALTER TABLE delegated_authorities DROP COLUMN IF EXISTS delegated_actions;

DROP POLICY IF EXISTS tenant_isolation_policy ON delegated_authorities;
CREATE POLICY tenant_isolation_policy ON delegated_authorities
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
