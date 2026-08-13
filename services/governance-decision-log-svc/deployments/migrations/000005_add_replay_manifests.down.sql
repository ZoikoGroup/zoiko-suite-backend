-- 000005_add_replay_manifests.down.sql
-- Drops all objects created by 000005_add_replay_manifests.up.sql.

DROP TRIGGER IF EXISTS replay_manifests_immutable ON replay_manifests;
DROP FUNCTION IF EXISTS reject_replay_manifest_mutation();
DROP TABLE IF EXISTS replay_manifests;
