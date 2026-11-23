-- SPEC_DEVIATIONS A1: ListPaymentAttempts is scoped to (tenant_id via RLS,
-- legal_entity_id) and keyset-paginated over (created_at, attempt_id) DESC.
-- The only existing index on this table is a bare tenant_id one
-- (000001_initial_schema), so this query would otherwise scan every attempt
-- in the tenant to find one legal entity's.

CREATE INDEX idx_payment_initiation_attempts_entity_list
    ON payment_initiation_attempts (tenant_id, legal_entity_id, created_at DESC, attempt_id DESC);
