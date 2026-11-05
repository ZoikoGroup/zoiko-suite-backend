-- Migration 000046: put every action evidence-requirements-svc checks into a
-- role template (R5), and describe the ones 000008 registered without one.
--
-- State before this migration: all nine actions were already registered --
-- EVIDENCE_REQUIREMENT_CREATE / _RETIRE and the four PBC actions
-- (EVIDENCE_REQUEST_CREATE, _MANAGE, _SUBMIT_RESPONSE, _EVALUATE) by 000008, as
-- STANDARD with an empty description; EVIDENCE_EVALUATE,
-- EVIDENCE_REQUIREMENT_READ and EVIDENCE_EVALUATION_READ by 000021. But no role
-- template carried the four PBC actions, so no role built from a template
-- could grant them. This migration registers nothing: it fills the six empty
-- descriptions (tiers stay as 000008 set them) and publishes the templates.

UPDATE permission_definitions p
   SET description = d.description
  FROM (VALUES
    ('EVIDENCE_REQUIREMENT_CREATE',      'Enforced by evidence-requirements-svc: create an evidence requirement in the catalogue'),
    ('EVIDENCE_REQUIREMENT_RETIRE',      'Enforced by evidence-requirements-svc: retire an evidence requirement by end-dating'),
    ('EVIDENCE_REQUEST_CREATE',          'Enforced by evidence-requirements-svc: create a provided-by-client evidence request'),
    ('EVIDENCE_REQUEST_MANAGE',          'Enforced by evidence-requirements-svc: send, reassign, extend, clarify, close a PBC request; read the PBC register'),
    ('EVIDENCE_REQUEST_SUBMIT_RESPONSE', 'Enforced by evidence-requirements-svc: submit an artifact response to a PBC request'),
    ('EVIDENCE_REQUEST_EVALUATE',        'Enforced by evidence-requirements-svc: mark a PBC request satisfied')
  ) AS d(action_name, description)
 WHERE p.action_name = d.action_name AND p.description = '';

-- A template for PBC request management. An evidence-request manager creates
-- requests, manages their lifecycle and evaluates responses, but cannot mutate
-- the catalogue (that stays with COMPLIANCE_CATALOGUE_ADMINISTRATOR).
INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('EVIDENCE_REQUEST_MANAGER', 'Evidence Request Manager', 'Create, send, reassign, extend and close evidence requests for the entity; evaluate responses and mark satisfied.', 'LEGAL_ENTITY', FALSE)
ON CONFLICT (template_code) DO NOTHING;

-- COMPLIANCE_CATALOGUE_ADMINISTRATOR v2 (a new version, never an edit of v1):
-- the same actions as v1, published now that every one is registered.
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('COMPLIANCE_CATALOGUE_ADMINISTRATOR', 2,
     ARRAY['OBLIGATION_DEFINITION_READ','OBLIGATION_DEFINITION_MANAGE','OBLIGATION_ENTITY_MAPPING_MANAGE',
           'EVIDENCE_REQUIREMENT_READ','EVIDENCE_REQUIREMENT_CREATE','EVIDENCE_REQUIREMENT_RETIRE'],
     'Republished with every evidence catalogue action registered', 'migration:000046')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- EVIDENCE_REQUEST_MANAGER v1
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('EVIDENCE_REQUEST_MANAGER', 1,
     ARRAY['EVIDENCE_REQUEST_CREATE','EVIDENCE_REQUEST_MANAGE','EVIDENCE_REQUEST_SUBMIT_RESPONSE','EVIDENCE_REQUEST_EVALUATE',
           'EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ'],
     'Initial template for PBC request management', 'migration:000046')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- OBLIGATION_MANAGER v2: adds the PBC evaluate action so obligation managers
-- can mark PBC requests satisfied (they already hold EVIDENCE_EVALUATE).
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
