-- Migration 000050: register iam.assignment.read and carry it in the IAM
-- administrator template (rule R5; tracker AC-2, 11 Nov 2026).
--
-- authorization-svc checks iam.assignment.read before answering for another
-- principal's assignments, suspensions and decisions (handler.go,
-- canonical_decisions.go, gov03_commands.go, validation.go,
-- assignment_suspension.go), and access-control-svc's review executor
-- (svc-access-control) holds it next to iam.assignment.revoke. It was never
-- registered here, so no governed role could carry it: only the dev seed and
-- authorization-svc migration 000043 grant it.
--
-- IAM_ACCESS_ADMINISTRATOR v2 = v1 (list, grant, revoke) + read. Versions are
-- append-only; v1 stays for roles instantiated from it.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('iam.assignment.read', 'TAXONOMY', 'STANDARD',
     'Enforced by authorization-svc: read another principal''s role assignments, suspensions and authorization decisions')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('IAM_ACCESS_ADMINISTRATOR', 2,
     ARRAY['iam.assignment.list','iam.assignment.grant','iam.assignment.revoke','iam.assignment.read'],
     'Adds iam.assignment.read (registered by 000050)', 'migration:000050')
ON CONFLICT (template_code, template_version) DO NOTHING;

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
