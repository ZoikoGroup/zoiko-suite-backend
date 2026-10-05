-- Seed for scripts/live_lifecycle_check.py: apply to a SCRATCH authorization-svc database
-- (never a shared one). alice holds payment approval with a 500.00 USD limit and the
-- delegation permissions; admin2 is a second administrator; carol holds PAYMENT_RELEASE,
-- which an SoD rule makes conflict with PAYMENT_APPROVE.
\set T '''7a000000-0000-4000-8000-000000000001'''
\set E '''7e000000-0000-4000-8000-000000000001'''
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id) VALUES
 ('70000000-0000-4000-8000-0000000000a1', :T, 'LIVE_FIN_LEAD', 'Finance lead', 'LEGAL_ENTITY', true, 'seed'),
 ('70000000-0000-4000-8000-0000000000a2', :T, 'LIVE_RELEASER', 'Releaser', 'LEGAL_ENTITY', true, 'seed');
INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag) VALUES
 ('70000000-0000-4000-8000-0000000000a1', 'LIVE_FIN', '["PAYMENT_APPROVE","PO_ISSUE","DELEGATION_CREATE","DELEGATION_VIEW","DELEGATION_REVOKE","DELEGATION_ADMINISTER"]', true),
 ('70000000-0000-4000-8000-0000000000a2', 'LIVE_REL', '["PAYMENT_RELEASE","DELEGATION_VIEW"]', true);
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by) VALUES
 ('alice',  '70000000-0000-4000-8000-0000000000a1', :E, now() - interval '1 day', 'seed'),
 ('admin2', '70000000-0000-4000-8000-0000000000a1', :E, now() - interval '1 day', 'seed'),
 ('carol',  '70000000-0000-4000-8000-0000000000a2', :E, now() - interval '1 day', 'seed');
INSERT INTO authority_limits (tenant_id, principal_id, authority_type, legal_entity_id, currency, upper_limit, effective_from) VALUES
 (:T, 'alice', 'PAYMENT_APPROVE', :E, 'USD', 500.00, now() - interval '1 day');
INSERT INTO sod_rules (domain_code, action_a, action_b, conflict_type, active_flag, tenant_id) VALUES
 ('PAYMENTS', 'PAYMENT_APPROVE', 'PAYMENT_RELEASE', 'STATIC', true, :T);
SELECT 'seeded';
