-- Repairs 000012 (Group 1 re-audit F-1, 19 Oct 2026).
--
-- 000012 added idempotency_key to the replay key. Adding a column to a unique
-- key makes it looser, not tighter: a retry under the same correlation_id that
-- carried a new Idempotency-Key stopped being a replay and became a second
-- insert. The case 000012 meant to cover (a repeated key under a fresh
-- correlation_id) is answered by the HTTP replay store (internal/idempotency)
-- before the handler runs, so the column does not belong in this index.
--
-- The column stays: it records the first request's key as evidence. Rows from
-- before 000012 hold NULL, which the store cannot scan into a string, so they
-- are backfilled and the column becomes NOT NULL DEFAULT ''.

UPDATE delegation_grants SET idempotency_key = '' WHERE idempotency_key IS NULL;
ALTER TABLE delegation_grants ALTER COLUMN idempotency_key SET DEFAULT '';
ALTER TABLE delegation_grants ALTER COLUMN idempotency_key SET NOT NULL;

DROP INDEX IF EXISTS idx_delegation_grants_idempotency;

CREATE UNIQUE INDEX idx_delegation_grants_idempotency
    ON delegation_grants (tenant_id, correlation_id, created_by_principal_id, action_type, legal_entity_id,
                          effective_from, effective_to);
