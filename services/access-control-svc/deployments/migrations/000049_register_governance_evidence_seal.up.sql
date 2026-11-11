-- Migration 000049: register GOVERNANCE_EVIDENCE_SEAL and a template for the
-- governance ledger's evidence steward.
--
-- governance-decision-log-svc seals its tenant's hash chain into an append-only
-- evidence package (G-07, POST /v1/evidence/packages/seal) only for a principal
-- holding GOVERNANCE_EVIDENCE_SEAL on the platform scope. The action was not
-- registered here, so no role could be built to grant it through the
-- governed role lifecycle; only a direct seed could. HIGH: a seal is a
-- permanent attestation that the ledger was intact at that point.
--
-- Template GOVERNANCE_EVIDENCE_STEWARD v1 (new): read the ledger, replay a
-- decision against its exact policy version, and seal the chain.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('GOVERNANCE_EVIDENCE_SEAL', 'LEGACY', 'HIGH',
     'Enforced by governance-decision-log-svc: seal the tenant''s decision hash chain into an append-only evidence package (G-07)')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('GOVERNANCE_EVIDENCE_STEWARD', 'Governance Evidence Steward',
     'Read the governance ledger, replay decisions and seal the evidence chain.', 'LEGAL_ENTITY', FALSE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('GOVERNANCE_EVIDENCE_STEWARD', 1,
     ARRAY['GOVERNANCE_DECISION_READ','GOVERNANCE_DECISION_REPLAY','GOVERNANCE_EVIDENCE_SEAL'],
     'Initial template for the governance evidence ledger (G-07)', 'migration:000049')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Same guard as 000021 / 000046 / 000047: every template action must be a
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
