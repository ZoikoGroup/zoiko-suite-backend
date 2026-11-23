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
-- explicit upgrade. Each new version is the template's latest version plus
-- the clock actions (000046/000047 already published OBLIGATION_MANAGER v2/v3
-- and COMPLIANCE_CATALOGUE_ADMINISTRATOR v2), so the next number and the
-- inherited actions are read, not assumed.
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by)
SELECT v.template_code, v.template_version + 1,
       ARRAY(SELECT DISTINCT a FROM unnest(v.permitted_actions || x.extra) AS a ORDER BY a),
       x.note, 'migration:000062'
  FROM (VALUES
        ('OBLIGATION_MANAGER', ARRAY['OBLIGATION_CLOCK_CREATE','OBLIGATION_CLOCK_SATISFY','OBLIGATION_CLOCK_RECALCULATE'],
         'Adds the WFC-05 deadline-clock actions (start, fulfil, recalculate)'),
        ('COMPLIANCE_CATALOGUE_ADMINISTRATOR', ARRAY['OBLIGATION_ESCALATION_POLICY_MANAGE'],
         'Adds clock escalation policies')
       ) AS x(code, extra, note)
  JOIN role_template_versions v ON v.template_code = x.code
 WHERE v.template_version = (SELECT max(template_version) FROM role_template_versions m WHERE m.template_code = x.code)
   AND NOT EXISTS (SELECT 1 FROM role_template_versions d WHERE d.template_code = x.code AND d.published_by = 'migration:000062');

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('OBLIGATION_AUTHORITY', 1, ARRAY['OBLIGATION_READ','OBLIGATION_CLOCK_WAIVE','OBLIGATION_CLOCK_NOT_APPLICABLE','APPLICABILITY_DECISION_READ'],
     'Initial template (WFC-05 waiver and not-applicable authority)', 'migration:000062')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- The three templates now carry the actions in their latest version; a
-- silent skip (ON CONFLICT on a taken number) fails the migration instead.
DO $$
DECLARE missing TEXT;
BEGIN
    SELECT string_agg(x.code || ':' || x.action, ', ') INTO missing
      FROM (VALUES ('OBLIGATION_MANAGER', 'OBLIGATION_CLOCK_CREATE'), ('OBLIGATION_MANAGER', 'OBLIGATION_CLOCK_SATISFY'),
                   ('OBLIGATION_MANAGER', 'OBLIGATION_CLOCK_RECALCULATE'),
                   ('COMPLIANCE_CATALOGUE_ADMINISTRATOR', 'OBLIGATION_ESCALATION_POLICY_MANAGE'),
                   ('OBLIGATION_AUTHORITY', 'OBLIGATION_CLOCK_WAIVE'), ('OBLIGATION_AUTHORITY', 'OBLIGATION_CLOCK_NOT_APPLICABLE')) AS x(code, action)
     WHERE NOT EXISTS (SELECT 1 FROM role_template_versions v
                        WHERE v.template_code = x.code
                          AND v.template_version = (SELECT max(template_version) FROM role_template_versions m WHERE m.template_code = x.code)
                          AND x.action = ANY (v.permitted_actions));
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION '000062: the latest template versions do not carry %', missing;
    END IF;
END
$$;

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
