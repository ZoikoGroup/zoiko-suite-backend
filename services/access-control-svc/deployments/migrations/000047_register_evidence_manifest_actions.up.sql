-- Migration 000047: register the two actions evidence-manifest-svc checks, and
-- put them in role templates (R5).
--
-- evidence-manifest-svc authorizes manifest generation on
-- EVIDENCE_MANIFEST_GENERATE and every manifest read on EVIDENCE_MANIFEST_READ,
-- at the manifest's legal entity (internal/handler/handler.go). Neither was
-- registered, so authorization-svc denied both with no_grant: no manifest could
-- be generated or read by anyone, and evidence-requirements-svc, which verifies
-- an EVIDENCE_MANIFEST reference by reading it as the evaluating principal,
-- failed closed (503) on every one.
--
-- Templates:
--   * EVIDENCE_MANIFEST_MANAGER v1 (new): generate and read manifests.
--   * EVIDENCE_REQUEST_MANAGER v2 and OBLIGATION_MANAGER v3: add READ. Both
--     evaluate evidence (EVIDENCE_REQUEST_EVALUATE / EVIDENCE_EVALUATE), and an
--     evaluation that offers a manifest reads it as the evaluating principal.
-- New versions, never edits of the published ones.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('EVIDENCE_MANIFEST_GENERATE', 'LEGACY', 'STANDARD', 'Enforced by evidence-manifest-svc: generate an evidence manifest for a legal entity'),
    ('EVIDENCE_MANIFEST_READ',     'LEGACY', 'STANDARD', 'Enforced by evidence-manifest-svc: read an evidence manifest and its records; evidence-requirements-svc reads one as the evaluating principal to verify an EVIDENCE_MANIFEST reference')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('EVIDENCE_MANIFEST_MANAGER', 'Evidence Manifest Manager', 'Generate and read evidence manifests for the entity.', 'LEGAL_ENTITY', FALSE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('EVIDENCE_MANIFEST_MANAGER', 1,
     ARRAY['EVIDENCE_MANIFEST_GENERATE','EVIDENCE_MANIFEST_READ'],
     'Initial template for evidence manifests', 'migration:000047')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- EVIDENCE_REQUEST_MANAGER v2: v1 plus EVIDENCE_MANIFEST_READ.
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('EVIDENCE_REQUEST_MANAGER', 2,
     ARRAY['EVIDENCE_REQUEST_CREATE','EVIDENCE_REQUEST_MANAGE','EVIDENCE_REQUEST_SUBMIT_RESPONSE','EVIDENCE_REQUEST_EVALUATE',
           'EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ','EVIDENCE_MANIFEST_READ'],
     'Adds EVIDENCE_MANIFEST_READ: an evaluation that offers a manifest reads it as this principal', 'migration:000047')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- OBLIGATION_MANAGER v3: v2 plus EVIDENCE_MANIFEST_READ.
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('OBLIGATION_MANAGER', 3,
     ARRAY['OBLIGATION_READ','OBLIGATION_CREATE','OBLIGATION_UPDATE','OBLIGATION_STATUS_UPDATE',
           'FILING_REQUIREMENT_READ','FILING_REQUIREMENT_CREATE','FILING_REQUIREMENT_STATUS_UPDATE',
           'OBLIGATION_DEFINITION_READ','EVIDENCE_EVALUATE','EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ',
           'EVIDENCE_REQUEST_EVALUATE','EVIDENCE_MANIFEST_READ'],
     'Adds EVIDENCE_MANIFEST_READ: an evaluation that offers a manifest reads it as this principal', 'migration:000047')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Same guard as 000021 / 000046: every template action must be a registered,
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
