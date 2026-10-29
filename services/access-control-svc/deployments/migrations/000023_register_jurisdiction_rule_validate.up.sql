-- Migration 000023: make POST /v1/validate-action grantable (Doc 03 §8.2 J-06,
-- 23 Oct 2026).
--
-- jurisdiction-rules-svc authorizes the route as JURISDICTION_RULE_VALIDATE.
-- The action was never registered, so no bundle could name it and every call
-- answered 403 — the same failure mode as POST /v1/validate-action before it
-- broke, recorded in the plan (§2 R5).
--
-- No template in this tree carried any JURISDICTION_* action, so there was no
-- template to extend. 000023 creates the compliance officer's template and
-- gives it the jurisdiction reads and the validation check. The check is a
-- read: it evaluates rules and writes nothing, so STANDARD, not HIGH.
--
-- It is not added to VIEWER_AUDITOR_READ_ONLY: validation is a POST, and the
-- read-only archetype is deliberately free of any verb that could be mistaken
-- for one.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('JURISDICTION_RULE_VALIDATE', 'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: evaluate whether an action is permitted under the effective jurisdiction rules (writes nothing)')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('JURISDICTION_COMPLIANCE_OFFICER', 'Jurisdiction Compliance Officer', 'Read the jurisdiction registry and test proposed actions against the effective rule pack; changes no rule.', 'TENANT', FALSE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('JURISDICTION_COMPLIANCE_OFFICER', 1, ARRAY['JURISDICTION_SUPPORT_VIEW','JURISDICTION_RULE_VALIDATE'],
                                            'Initial template (J-06: validate-action had no grant path)', 'migration:000023')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Same guard as 000009 and 000021: every template action is a registered,
-- active permission.
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
