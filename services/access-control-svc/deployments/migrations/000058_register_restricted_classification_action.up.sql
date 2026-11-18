-- Migration 000058: register the restricted-classification action (BIZ-02), enforced by document-vault-svc.
-- Confirming a classification whose value is RESTRICTED, and superseding a classification into or out of
-- RESTRICTED, needs this action in addition to the ordinary CONFIRM_CLASSIFICATION or SUPERSEDE_CLASSIFICATION
-- (the spec's security/privacy owner for restricted classes). It decides who may mark a record as restricted
-- or release it from that class, so it is HIGH. Who holds it is a role-design decision, so no role template
-- is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('CONFIRM_RESTRICTED_CLASSIFICATION', 'LEGACY', 'HIGH', 'Enforced by document-vault-svc: confirm, or supersede into or out of, a RESTRICTED record classification (security/privacy owner), in addition to the ordinary classification action')
ON CONFLICT (action_name) DO NOTHING;
