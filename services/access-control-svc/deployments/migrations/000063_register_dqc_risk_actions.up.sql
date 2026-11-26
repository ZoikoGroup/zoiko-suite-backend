-- Migration 000063: register the governed risk-acceptance actions enforced by data-quality-svc (DQC-05 section 8).
-- Asking for a risk to be accepted is a normal act; approving it is not: two people holding DQC_RISK_APPROVE, neither the
-- requester, accept one risk, and the certifier is a further person. Who holds them is a role-design decision, so no role
-- template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('DQC_RISK_REQUEST', 'LEGACY', 'STANDARD', 'Enforced by data-quality-svc: request that the risk of a failed mandatory rule be accepted'),
    ('DQC_RISK_APPROVE', 'LEGACY', 'CRITICAL', 'Enforced by data-quality-svc: approve a risk acceptance (two independent approvers; never the requester or the certifier)')
ON CONFLICT (action_name) DO NOTHING;
