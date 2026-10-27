-- Migration 000021: register the actions the Governance Platform and identity
-- services now check, and give them role templates (Domains 1 + 2 completion,
-- 21 Oct 2026).
--
-- A bundle may only name registered actions, so until these exist every gate
-- that asks for one can only answer DENIED. Three audit holds name this
-- directly:
--   H-1 (Governance, workflow row 22)  WORKFLOW_CANCEL / _ESCALATE /
--        _INVALIDATE / _ADMINISTER were unregistered, so the three lifecycle
--        endpoints answered 403 to everyone.
--   H-2 (workflow row 19)  WORKFLOW_APPROVE was registered (000008) but no
--        role template carried it, so a fresh deployment could approve
--        nothing until someone granted it by hand.
--   S1-B2 (Identity)  no role carried the IDENTITY_* actions identity-context
--        checks; four of them were not even registered.
--
-- Risk tiers: WORKFLOW_ADMINISTER acts on any workflow in the tenant and
-- WORKFLOW_DEFINE sets the approval route every later instance follows, so
-- both are HIGH (an assignment needs independent approval).
-- IDENTITY_PRINCIPAL_STATUS_SET suspends or reactivates a person and
-- IDENTITY_SESSION_INVALIDATE ends their sessions: HIGH. The rest are
-- STANDARD.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('WORKFLOW_READ',                    'LEGACY', 'STANDARD', 'Enforced by workflow-svc: read workflow instances, their evidence and definitions (participants read their own without it)'),
    ('WORKFLOW_CANCEL',                  'LEGACY', 'STANDARD', 'Enforced by workflow-svc: the initiator withdraws their own workflow'),
    ('WORKFLOW_ESCALATE',                'LEGACY', 'STANDARD', 'Enforced by workflow-svc: the initiator or a current approver escalates a workflow'),
    ('WORKFLOW_ESCALATE_TO',             'LEGACY', 'STANDARD', 'Enforced by workflow-svc: escalate a workflow to a named owner'),
    ('WORKFLOW_INVALIDATE',              'LEGACY', 'STANDARD', 'Enforced by workflow-svc: invalidate an approval when its bound subject changes (the subject owner''s service)'),
    ('WORKFLOW_ADMINISTER',              'LEGACY', 'HIGH',     'Enforced by workflow-svc: cancel, escalate or reopen a workflow the caller takes no part in'),
    ('WORKFLOW_DEFINE',                  'LEGACY', 'HIGH',     'Enforced by workflow-svc: create, supersede, activate, suspend or retire a workflow definition'),
    ('OBLIGATION_DEFINITION_READ',       'LEGACY', 'STANDARD', 'Enforced by obligations-svc: read obligation definitions and their entity mappings'),
    ('OBLIGATION_DEFINITION_MANAGE',     'LEGACY', 'STANDARD', 'Enforced by obligations-svc: create and version obligation definitions'),
    ('OBLIGATION_ENTITY_MAPPING_MANAGE', 'LEGACY', 'STANDARD', 'Enforced by obligations-svc: map an obligation definition to a legal entity'),
    ('FILING_REQUIREMENT_STATUS_UPDATE', 'LEGACY', 'STANDARD', 'Enforced by obligations-svc: move a filing requirement through its lifecycle'),
    ('EVIDENCE_EVALUATE',                'LEGACY', 'STANDARD', 'Enforced by evidence-requirements-svc: evaluate whether required evidence exists for an action'),
    ('EVIDENCE_REQUIREMENT_READ',        'LEGACY', 'STANDARD', 'Enforced by evidence-requirements-svc: read evidence requirements'),
    ('EVIDENCE_EVALUATION_READ',         'LEGACY', 'STANDARD', 'Enforced by evidence-requirements-svc: read evidence evaluations'),
    ('IDENTITY_SESSION_READ',            'LEGACY', 'STANDARD', 'Enforced by identity-context-svc: read another principal''s sessions'),
    ('IDENTITY_SESSION_INVALIDATE',      'LEGACY', 'HIGH',     'Enforced by identity-context-svc: end another principal''s sessions'),
    ('IDENTITY_PRINCIPAL_READ',          'LEGACY', 'STANDARD', 'Enforced by identity-context-svc: read another principal''s identity record'),
    ('IDENTITY_PRINCIPAL_STATUS_SET',    'LEGACY', 'HIGH',     'Enforced by identity-context-svc: suspend or reactivate a principal')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('WORKFLOW_APPROVER',          'Workflow Approver',          'Decide the approval stages routed to the holder within authority; never the holder''s own request.', 'LEGAL_ENTITY', FALSE),
    ('WORKFLOW_REQUESTER',         'Workflow Requester',         'Follow, chase and withdraw the holder''s own approval requests.', 'LEGAL_ENTITY', FALSE),
    ('WORKFLOW_ADMINISTRATOR',     'Workflow Administrator',     'Maintain approval routes and unblock workflows the holder takes no part in; decides nothing.', 'TENANT', FALSE),
    ('OBLIGATION_MANAGER',         'Obligation Manager',         'Track and fulfil the entity''s obligations and filings, and evaluate their evidence.', 'LEGAL_ENTITY', FALSE),
    ('COMPLIANCE_CATALOGUE_ADMINISTRATOR', 'Compliance Catalogue Administrator', 'Maintain the obligation and evidence-requirement catalogues the tenant''s entities are held to.', 'TENANT', FALSE),
    ('IDENTITY_SUPPORT_OPERATOR',  'Identity Support Operator',  'Attach and revoke support contexts and explain a principal''s identity context; approves nothing.', 'TENANT', FALSE),
    ('IDENTITY_SUPPORT_APPROVER',  'Identity Support Approver',  'Approve and review support contexts others raised (maker-checker with the operator).', 'TENANT', FALSE),
    ('IDENTITY_ADMINISTRATOR',     'Identity Administrator',     'End sessions, suspend or reactivate principals, reset a lost second factor, refresh identity caches.', 'TENANT', FALSE)
ON CONFLICT (template_code) DO NOTHING;

-- WORKFLOW_INVALIDATE is in no template on purpose: it belongs to the service
-- that owns the approved subject (invoice-approval-svc and the like), granted
-- to that service's principal, not to a person.
INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('WORKFLOW_APPROVER',          1, ARRAY['WORKFLOW_APPROVE','WORKFLOW_READ','WORKFLOW_ESCALATE'],
                                      'Initial template (Governance hold H-2)', 'migration:000021'),
    ('WORKFLOW_REQUESTER',         1, ARRAY['WORKFLOW_READ','WORKFLOW_CANCEL','WORKFLOW_ESCALATE'],
                                      'Initial template (Governance hold H-1)', 'migration:000021'),
    ('WORKFLOW_ADMINISTRATOR',     1, ARRAY['WORKFLOW_ADMINISTER','WORKFLOW_ESCALATE_TO','WORKFLOW_DEFINE','WORKFLOW_READ'],
                                      'Initial template (Governance hold H-1)', 'migration:000021'),
    ('OBLIGATION_MANAGER',         1, ARRAY['OBLIGATION_READ','OBLIGATION_CREATE','OBLIGATION_UPDATE','OBLIGATION_STATUS_UPDATE',
                                            'FILING_REQUIREMENT_READ','FILING_REQUIREMENT_CREATE','FILING_REQUIREMENT_STATUS_UPDATE',
                                            'OBLIGATION_DEFINITION_READ','EVIDENCE_EVALUATE','EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ'],
                                      'Initial template', 'migration:000021'),
    ('COMPLIANCE_CATALOGUE_ADMINISTRATOR', 1, ARRAY['OBLIGATION_DEFINITION_READ','OBLIGATION_DEFINITION_MANAGE','OBLIGATION_ENTITY_MAPPING_MANAGE',
                                            'EVIDENCE_REQUIREMENT_READ','EVIDENCE_REQUIREMENT_CREATE','EVIDENCE_REQUIREMENT_RETIRE'],
                                      'Initial template', 'migration:000021'),
    ('IDENTITY_SUPPORT_OPERATOR',  1, ARRAY['IDENTITY_SUPPORT_CONTEXT_ATTACH','IDENTITY_SUPPORT_CONTEXT_REVOKE','IDENTITY_CONTEXT_EXPLAIN',
                                            'IDENTITY_SESSION_READ','IDENTITY_PRINCIPAL_READ'],
                                      'Initial template (Identity hold S1-B2)', 'migration:000021'),
    ('IDENTITY_SUPPORT_APPROVER',  1, ARRAY['IDENTITY_SUPPORT_CONTEXT_APPROVE','IDENTITY_SUPPORT_CONTEXT_REVIEW',
                                            'IDENTITY_SESSION_READ','IDENTITY_PRINCIPAL_READ'],
                                      'Initial template (Identity hold S1-B2)', 'migration:000021'),
    ('IDENTITY_ADMINISTRATOR',     1, ARRAY['IDENTITY_SESSION_READ','IDENTITY_SESSION_INVALIDATE','IDENTITY_PRINCIPAL_READ','IDENTITY_PRINCIPAL_STATUS_SET',
                                            'IDENTITY_MFA_RESET','IDENTITY_CONTEXT_CACHE_REFRESH','IDENTITY_CONTEXT_TENANT_INVALIDATE'],
                                      'Initial template (Identity hold S1-B2)', 'migration:000021'),
    -- The read-only archetype reads the governance platform too. A new
    -- version, never an edit: roles on version 1 move only by an explicit
    -- upgrade.
    ('VIEWER_AUDITOR_READ_ONLY',   2, ARRAY['audit_evidence.read','audit_evidence.list','audit_evidence.export',
                                            'WORKFLOW_READ','OBLIGATION_READ','OBLIGATION_DEFINITION_READ','FILING_REQUIREMENT_READ',
                                            'EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ','GOVERNANCE_DECISION_READ','POLICY_READ'],
                                      'Adds the governance-platform reads', 'migration:000021')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Same guard as 000009: every template action is a registered, active
-- permission, so a typo fails the migration instead of shipping a role that
-- grants nothing.
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
