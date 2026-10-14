-- 000006_idempotency_principal_scope.up.sql
-- Idempotency scope is (tenant, principal, key). §18.1 requires an actor on every
-- decision, so a key used by one principal must never replay a decision
-- attributed to another. (The specification does not state the scope; this is
-- a documented service decision.)
--
-- Existing rows are backfilled from the actor recorded in the stored response
-- body; rows whose body has no actor keep principal_id = '' and can only be
-- replayed by a request that carries no principal, which the API forbids — so
-- they are inert, never replayed across principals.

ALTER TABLE decision_idempotency_keys
    ADD COLUMN IF NOT EXISTS principal_id TEXT NOT NULL DEFAULT '';

UPDATE decision_idempotency_keys
   SET principal_id = COALESCE(
        (CASE WHEN convert_from(response_body, 'UTF8') LIKE '{%'
              THEN convert_from(response_body, 'UTF8')::jsonb ->> 'actor_principal_id'
         END), '')
 WHERE principal_id = '';

ALTER TABLE decision_idempotency_keys
    DROP CONSTRAINT IF EXISTS decision_idempotency_keys_pkey;
ALTER TABLE decision_idempotency_keys
    ADD CONSTRAINT decision_idempotency_keys_pkey PRIMARY KEY (tenant_id, principal_id, idempotency_key);
