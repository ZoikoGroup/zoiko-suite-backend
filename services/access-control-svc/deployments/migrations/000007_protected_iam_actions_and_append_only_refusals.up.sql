-- Migration 000007: the protected catalogue gains the iam.* platform-admin
-- actions, and the refusal evidence becomes append-only under FORCE RLS.
--
-- 1. protected_permissions held only the eight pre-taxonomy names. Since 30 Sep
--    authorization-svc gates every admin write on the namespaced iam.* actions
--    (Authorization Standard §5), so a tenant bundle granting iam.role.manage
--    or iam.assignment.grant would hand its holders the power to define roles
--    and assign them, which §9 reserves to platform administration. Until
--    this release the catalogue was also never read (the handler called a
--    route nothing serves), so this list is now enforced for the first time.
INSERT INTO protected_permissions (action_name, description, category) VALUES
    ('iam.role.manage',              'Define and change roles in authorization-svc',         'platform_admin'),
    ('iam.permission_bundle.manage', 'Attach and change permission bundles',                 'platform_admin'),
    ('iam.assignment.grant',         'Assign roles to principals',                           'platform_admin'),
    ('iam.assignment.revoke',        'Revoke role assignments',                              'platform_admin'),
    ('iam.sod_rule.manage',          'Change segregation-of-duties rules',                   'platform_admin'),
    ('iam.abac_rule.manage',         'Change attribute-based access rules',                  'platform_admin'),
    ('iam.delegation.grant',         'Grant delegations through the admin API',              'platform_admin'),
    ('iam.delegation.revoke',        'Revoke delegations through the admin API',             'platform_admin'),
    ('iam.pam.manage',               'Manage privileged access',                             'platform_admin'),
    ('iam.break_glass.manage',       'Manage break-glass access',                            'platform_admin'),
    ('iam.support.manage',           'Manage support access to tenants',                     'platform_admin')
ON CONFLICT (action_name) DO UPDATE SET
    description = EXCLUDED.description,
    category    = EXCLUDED.category,
    active_flag = TRUE,
    updated_at  = now();

-- 2. FORCE RLS. Both tables had ENABLE only, so a service connecting as the
--    table owner bypassed every policy. role_definitions and
--    permission_bundle_defs have been FORCE since 000002.
ALTER TABLE protected_permissions FORCE ROW LEVEL SECURITY;
ALTER TABLE refused_escalations   FORCE ROW LEVEL SECURITY;

-- 3. Refusals are evidence (Doc 04 §20, "denials are as important as grants"),
--    so they are append-only. The old policy was FOR ALL, which let a tenant
--    UPDATE or DELETE its own refusal record.
DROP POLICY IF EXISTS tenant_isolation_policy ON refused_escalations;
DROP POLICY IF EXISTS refused_escalations_read ON refused_escalations;
DROP POLICY IF EXISTS refused_escalations_append ON refused_escalations;
CREATE POLICY refused_escalations_read ON refused_escalations FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY refused_escalations_append ON refused_escalations FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Grants are belt and braces on top of the policies, and guarded: the app role
-- does not exist on every database this runs against.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app') THEN
        REVOKE UPDATE, DELETE, TRUNCATE ON refused_escalations FROM zoiko_app;
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON protected_permissions FROM zoiko_app;
    END IF;
END
$$;
