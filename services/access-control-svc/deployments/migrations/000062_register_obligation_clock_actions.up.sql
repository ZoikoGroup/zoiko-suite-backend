-- Migration 000062: register the deadline-clock actions obligations-svc checks
-- (R-001 WFC-05, tracker D2-4 OB-S5..OB-S12, 23 Nov 2026) and carry them in
-- role templates (rule R5).
--
-- A waiver or a not-applicable decision removes a legal deadline, so both are
-- HIGH and sit in their own template (OBLIGATION_AUTHORITY), never in the
-- manager's: the person who runs the obligation is not the one who excuses it
-- (R-001 s7.1, NP-18). Escalation policies are a tenant-wide catalogue, like
-- obligation definitions.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('OBLIGATION_CLOCK_CREATE',            'LEGACY', 'STANDARD', 'Enforced by obligations-svc: start a deadline clock computed by jurisdiction-rules-svc'),
    ('OBLIGATION_CLOCK_SATISFY',           'LEGACY', 'STANDARD', 'Enforced by obligations-svc: fulfil a deadline clock with evidence'),
    ('OBLIGATION_CLOCK_RECALCULATE',       'LEGACY', 'STANDARD', 'Enforced by obligations-svc: recalculate a deadline clock (supersedes it when the due instant changes)'),
    ('OBLIGATION_CLOCK_WAIVE',             'LEGACY', 'HIGH',     'Enforced by obligations-svc: waive a deadline clock under an approved waiver workflow'),
    ('OBLIGATION_CLOCK_NOT_APPLICABLE',    'LEGACY', 'HIGH',     'Enforced by obligations-svc: mark a deadline clock not applicable under an approved workflow and an applicability decision'),
    ('OBLIGATION_ESCALATION_POLICY_MANAGE','LEGACY', 'STANDARD', 'Enforced by obligations-svc: create and retire versioned clock escalation policies')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('OBLIGATION_AUTHORITY', 'Obligation Authority',
     'Waive a deadline or declare it not applicable, after a second person approved the waiver; runs no obligation.', 'LEGAL_ENTITY', FALSE)
ON CONFLICT (template_code) DO NOTHING;

-- New versions, never edits: roles on an earlier version move only by an
-- explicit upgrade.
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('OBLIGATION_MANAGER', 2, ARRAY['OBLIGATION_READ','OBLIGATION_CREATE','OBLIGATION_UPDATE','OBLIGATION_STATUS_UPDATE',
                                    'FILING_REQUIREMENT_READ','FILING_REQUIREMENT_CREATE','FILING_REQUIREMENT_STATUS_UPDATE',
                                    'OBLIGATION_DEFINITION_READ','EVIDENCE_EVALUATE','EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ',
                                    'OBLIGATION_CLOCK_CREATE','OBLIGATION_CLOCK_SATISFY','OBLIGATION_CLOCK_RECALCULATE'],
     'Adds the WFC-05 deadline-clock actions (start, fulfil, recalculate)', 'migration:000062'),
    ('OBLIGATION_AUTHORITY', 1, ARRAY['OBLIGATION_READ','OBLIGATION_CLOCK_WAIVE','OBLIGATION_CLOCK_NOT_APPLICABLE','APPLICABILITY_DECISION_READ'],
     'Initial template (WFC-05 waiver and not-applicable authority)', 'migration:000062'),
    ('COMPLIANCE_CATALOGUE_ADMINISTRATOR', 2, ARRAY['OBLIGATION_DEFINITION_READ','OBLIGATION_DEFINITION_MANAGE','OBLIGATION_ENTITY_MAPPING_MANAGE',
                                    'EVIDENCE_REQUIREMENT_READ','EVIDENCE_REQUIREMENT_CREATE','EVIDENCE_REQUIREMENT_RETIRE',
                                    'OBLIGATION_ESCALATION_POLICY_MANAGE'],
     'Adds clock escalation policies', 'migration:000062')
ON CONFLICT (template_code, template_version) DO NOTHING;

DO $$
DECLARE missing TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO missing
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE v.published_by = 'migration:000062'
       AND NOT EXISTS (SELECT 1 FROM permission_definitions p WHERE p.action_name = a AND p.active_flag);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION '000062: template names unregistered or inactive actions: %', missing;
    END IF;
END
$$;
