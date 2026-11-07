-- Migration 000028: register the actions of the ZS-JUR-001 s23/s31 remediation workflow of jurisdiction-rules-svc. When a pack version is withdrawn or rolled
-- back, the decisions already made with it are tracked in a remediation case; each needs a disposition with evidence, and a case is closed by someone other
-- than the person who opened it. Opening and disposing are separate from closing so that the person who answers for the outcomes is not the person who
-- signs the case off. Who holds them is a role-design decision for Tax and Security leadership, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('REMEDIATION_CASE_OPEN',    'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: open a remediation case for decisions made with a pack version (withdrawals and rollbacks open one automatically)'),
    ('REMEDIATION_CASE_VIEW',    'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: read remediation cases, their decisions and dispositions'),
    ('REMEDIATION_CASE_DISPOSE', 'LEGACY', 'STANDARD', 'Enforced by jurisdiction-rules-svc: record what was done about a decision in a remediation case (reconfirmed, not affected, recalculated, superseded), with evidence'),
    ('REMEDIATION_CASE_CLOSE',   'LEGACY', 'HIGH',     'Enforced by jurisdiction-rules-svc: close a remediation case once every decision has a disposition (never the person who opened it)')
ON CONFLICT (action_name) DO NOTHING;
