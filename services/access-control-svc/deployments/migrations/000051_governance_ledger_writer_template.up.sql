-- Migration 000051: a role template carrying GOVERNANCE_DECISION_RECORD
-- (rule R5; tracker AC-2 / D2-2 VC-1, 11 Nov 2026).
--
-- governance-decision-log-svc checks GOVERNANCE_DECISION_RECORD on every
-- POST /v1/decisions. The action has been registered since the ledger was
-- built, but no template carried it, so a governed role could not be
-- instantiated for a decision writer: the ledger relays (svc-policy,
-- svc-workflow) get it from authorization-svc migration 000043, and a human
-- recorder only from the dev seed.
--
-- Template GOVERNANCE_LEDGER_WRITER v1 (new): record governance decisions and
-- read them back (a writer that cannot read what it wrote cannot reconcile).

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('GOVERNANCE_LEDGER_WRITER', 'Governance Ledger Writer',
     'Record governance decisions in the decision log and read them back.', 'LEGAL_ENTITY', FALSE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('GOVERNANCE_LEDGER_WRITER', 1,
     ARRAY['GOVERNANCE_DECISION_RECORD','GOVERNANCE_DECISION_READ'],
     'Initial template for decision-log writers (R5)', 'migration:000051')
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
