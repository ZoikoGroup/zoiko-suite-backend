-- Migration 000022: register banking-connector-svc's BNK-04 read action.
--
-- banking-connector-svc now serves BNK-04 ListTransactions
-- (GET /v1/banking/canonical-transactions) and authorizes it against
-- bank.transaction.read, the spec's own name for the permission (ZS-SVC-E-001
-- BNK-04 table 24: "bank.transaction.read; bank.transaction.normalize;
-- mapping.exception.approve"). A bundle may only name a registered action, so
-- until this row exists no role can hold it and every call is DENIED.
--
-- TAXONOMY naming, as 000008 asks of new names. STANDARD: it reads canonical
-- bank transactions for one legal entity and changes nothing.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('bank.transaction.read', 'TAXONOMY', 'STANDARD',
     'Enforced by banking-connector-svc: list a legal entity''s NORMALIZED canonical bank transactions (BNK-04 ListTransactions)')
ON CONFLICT (action_name) DO NOTHING;
