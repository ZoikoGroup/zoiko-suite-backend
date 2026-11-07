-- Migration 000024: make siem-integration-svc's gates grantable (28 Oct 2026).
--
-- siem-integration-svc authorizes exporter creation as SIEM_EXPORTER_CREATE,
-- exporter reads (by id, and since 28 Oct the list a person makes) as
-- SIEM_EXPORTER_READ, and the security-event stream as SIEM_EVENT_READ. None
-- was registered, so no bundle could name them and every one of those routes
-- answered 403 to every person forever (plan §2 R5).
--
--   SIEM_EXPORTER_CREATE  HIGH: it decides where the tenant's security
--                         telemetry is sent, and stores that platform's
--                         credential.
--   SIEM_EXPORTER_READ    STANDARD: exporter names, platforms and endpoints
--                         (never the credential, which is not serialised).
--   SIEM_EVENT_READ       HIGH: the stream records authentication failures,
--                         certificate revocations and key operations — a map
--                         of where the tenant's defences are weakest.
--
-- No template carried any SIEM_* action, so 000024 adds two: an analyst who
-- reads exporters and events and configures nothing, and an administrator who
-- creates and reads exporters but does not read the event stream.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('SIEM_EXPORTER_CREATE', 'LEGACY', 'HIGH',     'Enforced by siem-integration-svc: register a SIEM exporter (where security events are sent, with its credential)'),
    ('SIEM_EXPORTER_READ',   'LEGACY', 'STANDARD', 'Enforced by siem-integration-svc: read SIEM exporters (never their credential)'),
    ('SIEM_EVENT_READ',      'LEGACY', 'HIGH',     'Enforced by siem-integration-svc: read the security-event stream sent to an exporter')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('SECURITY_OPERATIONS_ANALYST', 'Security Operations Analyst', 'Read the tenant''s SIEM exporters and security-event stream; configures nothing.', 'TENANT', FALSE),
    ('SECURITY_SIEM_ADMINISTRATOR', 'SIEM Administrator', 'Register and read SIEM exporters; does not read the security-event stream.', 'TENANT', FALSE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('SECURITY_OPERATIONS_ANALYST', 1, ARRAY['SIEM_EXPORTER_READ','SIEM_EVENT_READ'],
                                       'Initial template (SIEM reads had no grant path)', 'migration:000024'),
    ('SECURITY_SIEM_ADMINISTRATOR', 1, ARRAY['SIEM_EXPORTER_CREATE','SIEM_EXPORTER_READ'],
                                       'Initial template (SIEM exporter creation had no grant path)', 'migration:000024')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Same guard as 000009, 000021 and 000023: every template action is a
-- registered, active permission.
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
