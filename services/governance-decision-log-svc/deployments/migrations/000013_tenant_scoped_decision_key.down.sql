-- Migration: 000013_tenant_scoped_decision_key.down.sql
--
-- Restores the global decision_id key. Refuses while two tenants share a
-- decision_id, because the old key cannot hold both and dropping either one
-- would destroy governance evidence.

BEGIN;

-- FORCE row-level security binds the owner this runs as, so with no tenant set
-- the check below would see no rows and always pass. Lifted for the check only.
ALTER TABLE governance_decisions NO FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF EXISTS (SELECT decision_id FROM governance_decisions GROUP BY decision_id HAVING COUNT(*) > 1) THEN
        RAISE EXCEPTION 'cannot restore the global decision_id key: some decision_id is used by more than one tenant';
    END IF;
END $$;
ALTER TABLE governance_decisions FORCE ROW LEVEL SECURITY;

ALTER TABLE replay_manifests DROP CONSTRAINT IF EXISTS replay_manifests_decision_fkey;
ALTER TABLE idempotency_keys DROP CONSTRAINT IF EXISTS idempotency_keys_decision_fkey;

ALTER TABLE governance_decisions DROP CONSTRAINT governance_decisions_pkey;
ALTER TABLE governance_decisions ADD CONSTRAINT governance_decisions_pkey PRIMARY KEY (decision_id);

ALTER TABLE replay_manifests
    ADD CONSTRAINT replay_manifests_decision_id_fkey
    FOREIGN KEY (decision_id) REFERENCES governance_decisions (decision_id);
ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_decision_id_fkey
    FOREIGN KEY (decision_id) REFERENCES governance_decisions (decision_id);

COMMIT;
