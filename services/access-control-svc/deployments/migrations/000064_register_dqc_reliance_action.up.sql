-- Migration 000064: register the action data-quality-svc enforces for recording reliance on a certification
-- (ZS-SVC-Z-001 RB-05, DQC-017 blast radius). Recording that something relied on certified evidence is a routine act
-- of a downstream service, so it is STANDARD risk; reading the blast radius uses the existing DQ read action. Who holds
-- it is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('DQC_RELIANCE_RECORD', 'LEGACY', 'STANDARD', 'Enforced by data-quality-svc: record that something relied on a data-quality certification, so a later revocation can name what rested on it')
ON CONFLICT (action_name) DO NOTHING;
