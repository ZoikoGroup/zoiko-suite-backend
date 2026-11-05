-- Migration 000046: register the missing EVIDENCE actions checked by
-- evidence-requirements-svc and add them to role templates.
--
-- The 000021 migration registered EVIDENCE_EVALUATE, EVIDENCE_REQUIREMENT_READ,
-- EVIDENCE_EVALUATION_READ and put them in OBLIGATION_MANAGER and
-- COMPLIANCE_CATALOGUE_ADMINISTRATOR. However:
--   * EVIDENCE_REQUIREMENT_CREATE and EVIDENCE_REQUIREMENT_RETIRE were listed in
--     COMPLIANCE_CATALOGUE_ADMINISTRATOR's permitted_actions but NEVER inserted into
--     permission_definitions — so the template grants nothing for them.
--   * The four PBC actions (EVIDENCE_REQUEST_CREATE, EVIDENCE_REQUEST_MANAGE,
--     EVIDENCE_REQUEST_SUBMIT_RESPONSE, EVIDENCE_REQUEST_EVALUATE) were not
--     registered at all.
--
-- This migration fixes both gaps. Risk tier: CREATE/RETIRE are HIGH (catalogue
-- mutation). PBC actions are STANDARD (operational, scoped to the request's
-- legal entity).

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('EVIDENCE_REQUIREMENT_CREATE',   'LEGACY', 'HIGH',     'Enforced by evidence-requirements-svc: create an evidence requirement in the catalogue'),
    ('EVIDENCE_REQUIREMENT_RETIRE',   'LEGACY', 'HIGH',     'Enforced by evidence-requirements-svc: retire an evidence requirement by end-dating'),
    ('EVIDENCE_REQUEST_CREATE',       'LEGACY', 'STANDARD', 'Enforced by evidence-requirements-svc: create a provided-by-client evidence request'),
    ('EVIDENCE_REQUEST_MANAGE',       'LEGACY', 'STANDARD', 'Enforced by evidence-requirements-svc: send, reassign, extend, clarify, close a PBC request'),
    ('EVIDENCE_REQUEST_SUBMIT_RESPONSE', 'LEGACY', 'STANDARD', 'Enforced by evidence-requirements-svc: submit an artifact response to a PBC request'),
    ('EVIDENCE_REQUEST_EVALUATE',     'LEGACY', 'STANDARD', 'Enforced by evidence-requirements-svc: mark a PBC request satisfied or evaluate its response')
ON CONFLICT (action_name) DO NOTHING;

-- Add a template for PBC request management. An evidence-request-manager can
-- create requests, manage their lifecycle, and evaluate responses, but cannot
-- mutate the catalogue (that stays with COMPLIANCE_CATALOGUE_ADMINISTRATOR).
INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('EVIDENCE_REQUEST_MANAGER',    'Evidence Request Manager',    'Create, send, reassign, extend and close evidence requests for the entity; evaluate responses and mark satisfied.', 'LEGAL_ENTITY', FALSE)
ON CONFLICT (template_code) DO NOTHING;

-- Update COMPLIANCE_CATALOGUE_ADMINISTRATOR to version 2 (new version, never
-- edit v1) so it actually grants the catalogue mutation actions that were
-- already listed in its v1 permitted_actions but missing from
-- permission_definitions. The v1 actions that were already valid stay.
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('COMPLIANCE_CATALOGUE_ADMINISTRATOR', 2,
     ARRAY['OBLIGATION_DEFINITION_READ','OBLIGATION_DEFINITION_MANAGE','OBLIGATION_ENTITY_MAPPING_MANAGE',
           'EVIDENCE_REQUIREMENT_READ','EVIDENCE_REQUIREMENT_CREATE','EVIDENCE_REQUIREMENT_RETIRE'],
     'Adds EVIDENCE_REQUIREMENT_CREATE/RETIRE now that they are registered', 'migration:000046')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- EVIDENCE_REQUEST_MANAGER v1
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('EVIDENCE_REQUEST_MANAGER', 1,
     ARRAY['EVIDENCE_REQUEST_CREATE','EVIDENCE_REQUEST_MANAGE','EVIDENCE_REQUEST_SUBMIT_RESPONSE','EVIDENCE_REQUEST_EVALUATE',
           'EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ'],
     'Initial template for PBC request management', 'migration:000046')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- OBLIGATION_MANAGER v2: add the PBC evaluate action so obligation managers
-- can mark PBC requests satisfied (they already have EVIDENCE_EVALUATE etc).
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('OBLIGATION_MANAGER', 2,
     ARRAY['OBLIGATION_READ','OBLIGATION_CREATE','OBLIGATION_UPDATE','OBLIGATION_STATUS_UPDATE',
           'FILING_REQUIREMENT_READ','FILING_REQUIREMENT_CREATE','FILING_REQUIREMENT_STATUS_UPDATE',
           'OBLIGATION_DEFINITION_READ','EVIDENCE_EVALUATE','EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ',
           'EVIDENCE_REQUEST_EVALUATE'],
     'Adds EVIDENCE_REQUEST_EVALUATE for mark-satisfied', 'migration:000046')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Same guard as 000021: every template action must be a registered, active permission.
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