-- Migration: 000006_add_delegation_tenant.down.sql
--
-- Drops the policy and the row security before the column, because a policy
-- referencing tenant_id blocks the column being dropped.
--
-- This reopens the gap 000006 closed: without tenant_id there is no path to a
-- tenant to scope this table by, and every delegation becomes readable,
-- revocable and evaluable under any tenant's scope again.

DROP POLICY IF EXISTS tenant_isolation_policy ON delegated_authorities;

ALTER TABLE delegated_authorities NO FORCE ROW LEVEL SECURITY;
ALTER TABLE delegated_authorities DISABLE ROW LEVEL SECURITY;

DROP INDEX IF EXISTS idx_delegations_tenant_lookup;

ALTER TABLE delegated_authorities DROP COLUMN IF EXISTS tenant_id;
