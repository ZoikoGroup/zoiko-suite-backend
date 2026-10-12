-- Migration: 000013_tenant_scoped_decision_key.up.sql
--
-- Doc 04 §7.1 GovernanceDecision is tenant-owned, but 000001 made decision_id
-- a GLOBAL primary key. decision_id is caller-supplied, so a tenant posting an
-- id another tenant had already used got 409 decision_id_conflict -- which
-- told it that the id exists somewhere else on the platform. The key is now
-- (tenant_id, decision_id): each tenant has its own id namespace, and nothing
-- about another tenant's ids can be learned from a write.
--
-- No row can violate the new key: the old one was strictly narrower.

BEGIN;

-- Both the old and the new constraint names, so a database left half-way by
-- an interrupted down migration can run this file again.
ALTER TABLE replay_manifests DROP CONSTRAINT IF EXISTS replay_manifests_decision_id_fkey;
ALTER TABLE idempotency_keys DROP CONSTRAINT IF EXISTS idempotency_keys_decision_id_fkey;
ALTER TABLE replay_manifests DROP CONSTRAINT IF EXISTS replay_manifests_decision_fkey;
ALTER TABLE idempotency_keys DROP CONSTRAINT IF EXISTS idempotency_keys_decision_fkey;

ALTER TABLE governance_decisions DROP CONSTRAINT governance_decisions_pkey;
ALTER TABLE governance_decisions
    ADD CONSTRAINT governance_decisions_pkey PRIMARY KEY (tenant_id, decision_id);

ALTER TABLE replay_manifests
    ADD CONSTRAINT replay_manifests_decision_fkey
    FOREIGN KEY (tenant_id, decision_id) REFERENCES governance_decisions (tenant_id, decision_id);

ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_decision_fkey
    FOREIGN KEY (tenant_id, decision_id) REFERENCES governance_decisions (tenant_id, decision_id);

COMMIT;
