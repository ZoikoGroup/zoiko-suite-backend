-- Migration 000062 down. Refused while a role was made from one of its
-- template versions or a bundle still grants one of its actions.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT template_code, ', ') INTO v_used
      FROM role_definitions
     WHERE template_code = 'OBLIGATION_AUTHORITY'
        OR (template_code = 'OBLIGATION_MANAGER' AND template_version = 2)
        OR (template_code = 'COMPLIANCE_CATALOGUE_ADMINISTRATOR' AND template_version = 2);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000062 down refused: roles were made from: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a = ANY (ARRAY['OBLIGATION_CLOCK_CREATE','OBLIGATION_CLOCK_SATISFY','OBLIGATION_CLOCK_RECALCULATE',
                          'OBLIGATION_CLOCK_WAIVE','OBLIGATION_CLOCK_NOT_APPLICABLE','OBLIGATION_ESCALATION_POLICY_MANAGE']);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000062 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

-- role_template_versions is append-only by trigger (000009); a down is the one
-- sanctioned removal.
ALTER TABLE role_template_versions DISABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_template_versions WHERE published_by = 'migration:000062';
ALTER TABLE role_template_versions ENABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_templates WHERE template_code = 'OBLIGATION_AUTHORITY';
DELETE FROM permission_definitions
 WHERE action_name = ANY (ARRAY['OBLIGATION_CLOCK_CREATE','OBLIGATION_CLOCK_SATISFY','OBLIGATION_CLOCK_RECALCULATE',
                                'OBLIGATION_CLOCK_WAIVE','OBLIGATION_CLOCK_NOT_APPLICABLE','OBLIGATION_ESCALATION_POLICY_MANAGE']);
