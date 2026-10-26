-- Migration 000020: register payment-status-svc's BNK-07 actions.
--
-- payment-status-svc has enforced these four action names since its first
-- release, but none was ever registered here. A bundle may only name a
-- registered action, so no role could hold any of them and every BNK-07
-- authorization check was permanently DENIED. This matters now that
-- ConfirmSettlement and RecordRejection (BNK-07 A1) are real commands:
-- PAYMENT_FINALITY_CONFIRM is their gate.
--
-- READ/INGEST/RESOLVE are STANDARD, matching their sibling payment actions.
-- PAYMENT_FINALITY_CONFIRM is HIGH: it is the manual finality override the
-- spec requires to be an exceptional controlled workflow, and HIGH makes an
-- assignment of it need independent approval. The service still enforces the
-- creator-different SoD rule in addition to this permission.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('PAYMENT_STATUS_READ', 'LEGACY', 'STANDARD',
     'Enforced by payment-status-svc: read payment execution state and finality evidence'),
    ('PAYMENT_STATUS_INGEST', 'LEGACY', 'STANDARD',
     'Enforced by payment-status-svc: record, link or poll payment execution status'),
    ('PAYMENT_STATUS_RESOLVE', 'LEGACY', 'STANDARD',
     'Enforced by payment-status-svc: cancel a payment where supported'),
    ('PAYMENT_FINALITY_CONFIRM', 'LEGACY', 'HIGH',
     'Enforced by payment-status-svc: manually confirm final settlement or rejection with evidence')
ON CONFLICT (action_name) DO NOTHING;
