-- Migration 000057: register the counterparty read actions (BIZ-06), enforced by counterparty-management-svc.
-- The relationship, interaction and opportunity reads, and the consent-record read, used to be unauthorized
-- beyond the tenant; counterparty-management-svc now checks CRM_READ and, for the consent record,
-- CRM_CONSENT_READ. Until they are registered no bundle can name them, so those reads answer 403 in a real
-- deployment. Both are reads, so STANDARD. The consent read is kept separate because it returns the consent
-- record itself, so it is granted explicitly rather than with the ordinary read. Who holds them is a
-- role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('CRM_READ',         'LEGACY', 'STANDARD', 'Enforced by counterparty-management-svc: read relationships, interactions and opportunities of a legal entity'),
    ('CRM_CONSENT_READ', 'LEGACY', 'STANDARD', 'Enforced by counterparty-management-svc: read the consent record held against a counterparty relationship')
ON CONFLICT (action_name) DO NOTHING;
