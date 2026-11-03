-- Migration 000027: register identity-context-svc's federation gates and give
-- them role templates (IC-9, 3 Nov 2026).
--
-- With FEDERATION_ENABLED, identity-context-svc authorizes every federation
-- admin route at one of these six actions. None was registered, so no bundle
-- could name them and every one of those routes answered 403 to every person
-- (plan §2 R5) - found by the live probe with federation turned on.
--
--   FEDERATION_READ                 STANDARD: read the tenant's connections,
--                                   links, logins and provisioning status.
--   FEDERATION_CONFIGURE            HIGH: propose a connection or a new
--                                   version of one (issuer, keys, mapping).
--   FEDERATION_APPROVE              CRITICAL: approve or reject a proposed
--                                   version - it decides which external
--                                   identity provider the platform believes.
--   FEDERATION_SUSPEND              HIGH: suspend, resume or revoke a
--                                   connection; refresh or rotate its keys.
--   FEDERATION_LINK                 HIGH: bind an external subject to a
--                                   local principal.
--   FEDERATION_PROVISIONING_MANAGE  HIGH: SCIM tokens and group-to-role
--                                   mappings.
--
-- Maker-checker: the administrator template proposes (CONFIGURE) and the
-- approver template approves (APPROVE); no template carries both.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('FEDERATION_READ',                'LEGACY', 'STANDARD', 'Enforced by identity-context-svc: read the tenant''s federation connections, links and provisioning'),
    ('FEDERATION_CONFIGURE',           'LEGACY', 'HIGH',     'Enforced by identity-context-svc: propose a federation connection or a new version of one'),
    ('FEDERATION_APPROVE',             'LEGACY', 'CRITICAL', 'Enforced by identity-context-svc: approve or reject a proposed federation connection version'),
    ('FEDERATION_SUSPEND',             'LEGACY', 'HIGH',     'Enforced by identity-context-svc: suspend, resume or revoke a federation connection, or refresh its keys'),
    ('FEDERATION_LINK',                'LEGACY', 'HIGH',     'Enforced by identity-context-svc: link an external identity provider subject to a local principal'),
    ('FEDERATION_PROVISIONING_MANAGE', 'LEGACY', 'HIGH',     'Enforced by identity-context-svc: manage SCIM tokens and group-to-role mappings')
ON CONFLICT (action_name) DO NOTHING;

-- New versions, never edits: roles on earlier versions move only by an explicit upgrade.
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('IDENTITY_SUPPORT_OPERATOR',  3,
     ARRAY['IDENTITY_SUPPORT_CONTEXT_ATTACH','IDENTITY_SUPPORT_CONTEXT_REVOKE','IDENTITY_CONTEXT_EXPLAIN',
            'IDENTITY_SESSION_READ','IDENTITY_PRINCIPAL_READ','IDENTITY_AUTH_EVENTS_READ',
            'FEDERATION_READ'],
     'Adds federation reads', 'migration:000027'),
    ('IDENTITY_SUPPORT_APPROVER',  3,
     ARRAY['IDENTITY_SUPPORT_CONTEXT_APPROVE','IDENTITY_SUPPORT_CONTEXT_REVIEW',
            'IDENTITY_SESSION_READ','IDENTITY_PRINCIPAL_READ','IDENTITY_PASSWORD_RESET_APPROVE',
            'FEDERATION_READ','FEDERATION_APPROVE'],
     'Adds federation version approval (the checker of FEDERATION_CONFIGURE)', 'migration:000027'),
    ('IDENTITY_ADMINISTRATOR',     3,
     ARRAY['IDENTITY_SESSION_READ','IDENTITY_SESSION_INVALIDATE','IDENTITY_PRINCIPAL_READ',
            'IDENTITY_PRINCIPAL_STATUS_SET','IDENTITY_MFA_RESET','IDENTITY_CONTEXT_CACHE_REFRESH',
            'IDENTITY_CONTEXT_TENANT_INVALIDATE','IDENTITY_PRINCIPAL_CREATE',
            'IDENTITY_PASSWORD_RESET_REQUEST','IDENTITY_AUTH_EVENTS_READ','IDENTITY_SUBJECT_MERGE',
            'FEDERATION_READ','FEDERATION_CONFIGURE','FEDERATION_SUSPEND','FEDERATION_LINK',
            'FEDERATION_PROVISIONING_MANAGE'],
     'Adds federation configuration, suspension, links and provisioning (approval stays with the approver)', 'migration:000027')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Same guard as 000009, 000021, 000024 and 000026: every template action is a
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

-- Maker-checker holds in every template version: none both proposes and
-- approves a federation connection.
DO $$
DECLARE both_ TEXT;
BEGIN
    SELECT string_agg(template_code || ' v' || template_version, ', ') INTO both_
      FROM role_template_versions
     WHERE 'FEDERATION_CONFIGURE' = ANY (permitted_actions) AND 'FEDERATION_APPROVE' = ANY (permitted_actions);
    IF both_ IS NOT NULL THEN
        RAISE EXCEPTION 'a template both configures and approves federation: %', both_;
    END IF;
END
$$;
