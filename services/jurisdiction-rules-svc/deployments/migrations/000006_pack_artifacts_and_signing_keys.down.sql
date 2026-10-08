-- 000006 down: removes compiled artifacts and trusted signing keys.
DROP TABLE IF EXISTS pack_artifacts;
DROP TABLE IF EXISTS pack_signing_keys;
DROP FUNCTION IF EXISTS jur_pack_artifact_guard();
DROP FUNCTION IF EXISTS jur_signing_key_guard();
