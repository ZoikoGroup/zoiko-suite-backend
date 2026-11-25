-- Migration 000062: register the certification revocation action enforced by data-quality-svc (DQC-05, RB-05).
-- Withdrawing reliance on certified evidence is a high-risk act, separate from certifying and from managing runs.
-- Who holds it is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('DQC_CERTIFICATION_REVOKE', 'LEGACY', 'HIGH', 'Enforced by data-quality-svc: revoke a data-quality certification prospectively, preserving its sealed evidence')
ON CONFLICT (action_name) DO NOTHING;
