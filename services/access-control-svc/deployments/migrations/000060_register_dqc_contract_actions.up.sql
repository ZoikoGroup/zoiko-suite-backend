-- Migration 000060: register the data contract actions enforced by data-quality-svc (DQC-01, ZS-SVC-Z-001
-- section 5). Until an action is registered no bundle can name it, so its check is permanently denied.
--
-- Reading, authoring and publishing a contract are three separate actions: whoever may author a contract is
-- not thereby allowed to publish it (maker-checker). Who holds which is a role-design decision, so no role
-- template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('DQC_CONTRACT_READ',    'LEGACY', 'STANDARD', 'Enforced by data-quality-svc: read a data contract version and run a compatibility check'),
    ('DQC_CONTRACT_MANAGE',  'LEGACY', 'STANDARD', 'Enforced by data-quality-svc: create a contract draft and submit it for review'),
    ('DQC_CONTRACT_PUBLISH', 'LEGACY', 'HIGH',     'Enforced by data-quality-svc: publish, deprecate or retire a contract version (never the author or submitter of that version)')
ON CONFLICT (action_name) DO NOTHING;
