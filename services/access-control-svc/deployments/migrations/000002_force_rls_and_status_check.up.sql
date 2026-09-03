-- access-control-svc: make the tenant isolation policies load-bearing, and
-- constrain status to the vocabulary the domain actually defines.
--
-- Three separate defects, all of the shape this estate has hit before.
--
-- 1. ENABLE WITHOUT FORCE. 000001 wrote correct tenant_isolation policies and
--    ENABLEd row security, which exempts the table OWNER -- and the owner is
--    who runs migrations. Until the estate moved services off the Postgres
--    superuser, the policies never executed for anybody. They now execute for
--    the runtime role, but the owner is still exempt without FORCE, so anything
--    connecting as the migration user reads every tenant. FORCE closes that.
--
-- 2. USING WITHOUT WITH CHECK. A policy with only USING governs what rows are
--    VISIBLE, not what rows may be WRITTEN. A caller could therefore INSERT a
--    role definition attributed to another tenant and then be unable to see it
--    -- the row would sit in that tenant's catalogue, authored from outside it.
--    This is the same write-side gap supabase/README.md records against
--    obligations-svc. Both policies are recreated FOR ALL with USING and
--    WITH CHECK over the same predicate.
--
-- 3. STATUS WAS UNCONSTRAINED. status is VARCHAR(20) with the vocabulary in a
--    trailing comment, so any string persisted. domain.RoleStatus defines
--    exactly ACTIVE and RETIRED; the handler now rejects anything else, and
--    this constraint is the backstop for every other writer.
--
-- NOTE ON THE PREDICATE. tenant_id is VARCHAR, not uuid, so
-- current_setting('app.tenant_id', true) compares directly and the
-- empty-string-cast-to-uuid failure that needed NULLIF elsewhere in the estate
-- cannot arise here. NULLIF is still applied so an unset tenant is NULL rather
-- than '', which is not true and so matches nothing -- fail-closed by SQL
-- semantics rather than by a guard that could be removed.

-- ── 1 + 2. Real tenant isolation, on both sides of the read/write boundary ──

ALTER TABLE role_definitions FORCE ROW LEVEL SECURITY;
ALTER TABLE permission_bundle_defs FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON role_definitions;
CREATE POLICY tenant_isolation_policy ON role_definitions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation_policy ON permission_bundle_defs;
CREATE POLICY tenant_isolation_policy ON permission_bundle_defs FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- ── 3. status vocabulary ─────────────────────────────────────────────────────
--
-- NOT VALID: existing rows are not rewritten to satisfy a rule added after they
-- were written. Every row this service has ever created went through the
-- handler with a literal ACTIVE or RETIRED, so there is nothing to repair --
-- but a migration that can fail on historical data is a migration that blocks
-- a deploy, and the constraint binds every future write either way.
ALTER TABLE role_definitions
    ADD CONSTRAINT role_definitions_status_check
    CHECK (status IN ('ACTIVE', 'RETIRED')) NOT VALID;
