-- Migration 000060: register the signer-office actions, enforced by workflow-svc (ZS-GOV-INT-002 §4.2).
-- A signature-bound workflow asks authorization-svc, at the moment of each signature, which corporate offices the
-- signer holds in the legal entity: SIGNATORY_SIGN_AS_<OFFICE>. policy-svc's signatory matrix says which offices a
-- band needs; Authorization alone decides who holds them (Doc 01 §3: signer eligibility). An office no definition
-- registers can never be held, so a matrix naming it cannot be signed (fail closed). Signing binds the legal entity:
-- every action is HIGH. Who holds them is the entity's delegation of authority, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('SIGNATORY_SIGN_AS_DIRECTOR',          'LEGACY', 'HIGH', 'Enforced by workflow-svc: sign as a Director under a policy-svc signatory requirement'),
    ('SIGNATORY_SIGN_AS_CFO',               'LEGACY', 'HIGH', 'Enforced by workflow-svc: sign as the Chief Financial Officer under a policy-svc signatory requirement'),
    ('SIGNATORY_SIGN_AS_CEO',               'LEGACY', 'HIGH', 'Enforced by workflow-svc: sign as the Chief Executive Officer under a policy-svc signatory requirement'),
    ('SIGNATORY_SIGN_AS_FINANCE_SIGNER',    'LEGACY', 'HIGH', 'Enforced by workflow-svc: sign as a Finance-authorized signer under a policy-svc signatory requirement'),
    ('SIGNATORY_SIGN_AS_COMPANY_SECRETARY', 'LEGACY', 'HIGH', 'Enforced by workflow-svc: sign as the Company Secretary under a policy-svc signatory requirement'),
    ('SIGNATORY_SIGN_AS_GENERAL_COUNSEL',   'LEGACY', 'HIGH', 'Enforced by workflow-svc: sign as the General Counsel under a policy-svc signatory requirement'),
    ('SIGNATORY_SIGN_AS_TREASURER',         'LEGACY', 'HIGH', 'Enforced by workflow-svc: sign as the Treasurer under a policy-svc signatory requirement')
ON CONFLICT (action_name) DO NOTHING;
