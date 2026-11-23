-- Migration 000038: register the REF-01 import actions (ZS-SVC-I-001 s4.11), enforced by jurisdiction-rules-svc.
-- ImportJurisdictionUpdate is maker-checker: the principal who proposes an externally sourced update and the one who approves it must differ,
-- which jurisdiction-rules-svc enforces itself; these permissions decide who may take each side. Who holds them is a role-design decision,
-- so no role template is created here. VIEW is read-only access to proposed batches and their per-row errors.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('JURISDICTION_IMPORT_PROPOSE', 'LEGACY', 'HIGH', 'Enforced by jurisdiction-rules-svc: propose an externally sourced update to the jurisdiction registry'),
    ('JURISDICTION_IMPORT_APPROVE', 'LEGACY', 'HIGH', 'Enforced by jurisdiction-rules-svc: approve (applying every row) or reject a proposed jurisdiction import; never the proposer'),
    ('JURISDICTION_IMPORT_VIEW',    'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: read proposed and decided jurisdiction imports with their rows and errors')
ON CONFLICT (action_name) DO NOTHING;
