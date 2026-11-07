-- Migration 000026: register identity-context-svc's remaining gates and give
-- them role templates (IC-7, 2 Nov 2026).
--
-- identity-context-svc authorizes subject creation as
-- IDENTITY_PRINCIPAL_CREATE, administrator-initiated resets as
-- IDENTITY_PASSWORD_RESET_REQUEST, reset decisions as
-- IDENTITY_PASSWORD_RESET_APPROVE, authentication-event reads as
-- IDENTITY_AUTH_EVENTS_READ, and subject merges as IDENTITY_SUBJECT_MERGE.
-- None was registered, so no bundle could name them and every one of those
-- routes answered 403 to every person forever (plan §2 R5).
--
--   IDENTITY_PRINCIPAL_CREATE      HIGH: creates a tenant subject (a person or
--                                  workload) - a new identity on the platform.
--   IDENTITY_PASSWORD_RESET_REQUEST  HIGH: opens an administrator-initiated
--                                  reset awaiting independent approval.
--   IDENTITY_PASSWORD_RESET_APPROVE  HIGH: approves or rejects a reset -
--                                  independent approval, maker-checker.
--   IDENTITY_AUTH_EVENTS_READ      STANDARD: read authentication events (audit).
--   IDENTITY_SUBJECT_MERGE         HIGH: merges two subject identities into one.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('IDENTITY_PRINCIPAL_CREATE',      'LEGACY', 'HIGH',     'Enforced by identity-context-svc: create a tenant subject (person or workload)'),
    ('IDENTITY_PASSWORD_RESET_REQUEST','LEGACY', 'HIGH',     'Enforced by identity-context-svc: request an administrator-initiated password reset awaiting approval'),
    ('IDENTITY_PASSWORD_RESET_APPROVE','LEGACY', 'HIGH',     'Enforced by identity-context-svc: approve or reject an administrator-initiated password reset'),
    ('IDENTITY_AUTH_EVENTS_READ',      'LEGACY', 'STANDARD', 'Enforced by identity-context-svc: read authentication events for the tenant'),
    ('IDENTITY_SUBJECT_MERGE',         'LEGACY', 'HIGH',     'Enforced by identity-context-svc: merge two subject identities into a survivor')
ON CONFLICT (action_name) DO NOTHING;

-- New versions, never edits: roles on version 1 move only by an explicit upgrade.
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('IDENTITY_SUPPORT_OPERATOR',  2,
     ARRAY['IDENTITY_SUPPORT_CONTEXT_ATTACH','IDENTITY_SUPPORT_CONTEXT_REVOKE','IDENTITY_CONTEXT_EXPLAIN',
            'IDENTITY_SESSION_READ','IDENTITY_PRINCIPAL_READ','IDENTITY_AUTH_EVENTS_READ'],
     'Adds authentication-event reads and subject-merge visibility', 'migration:000026'),
    ('IDENTITY_SUPPORT_APPROVER',  2,
     ARRAY['IDENTITY_SUPPORT_CONTEXT_APPROVE','IDENTITY_SUPPORT_CONTEXT_REVIEW',
            'IDENTITY_SESSION_READ','IDENTITY_PRINCIPAL_READ','IDENTITY_PASSWORD_RESET_APPROVE'],
     'Adds password-reset approval (independent approval, maker-checker)', 'migration:000026'),
    ('IDENTITY_ADMINISTRATOR',     2,
     ARRAY['IDENTITY_SESSION_READ','IDENTITY_SESSION_INVALIDATE','IDENTITY_PRINCIPAL_READ',
            'IDENTITY_PRINCIPAL_STATUS_SET','IDENTITY_MFA_RESET','IDENTITY_CONTEXT_CACHE_REFRESH',
            'IDENTITY_CONTEXT_TENANT_INVALIDATE','IDENTITY_PRINCIPAL_CREATE',
            'IDENTITY_PASSWORD_RESET_REQUEST','IDENTITY_AUTH_EVENTS_READ','IDENTITY_SUBJECT_MERGE'],
     'Adds subject creation, reset requests, event reads and subject merge', 'migration:000026')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Same guard as 000009, 000021 and 000024: every template action is a
-- registered, active permission, so a typo fails the migration instead of
-- shipping a role that grants nothing.
DO $$
DECLARE missing TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO missing
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE NOT EXISTS (SELECT 1 FROM permission_definitions p WHERE p.action_name = a AND p.active_flag);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'role template actions not in permission_definitions: %', missing;
    END IF;
END
$$;
