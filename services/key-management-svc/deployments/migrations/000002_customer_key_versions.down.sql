-- Reverses 000002: drops the version history and the constraints it added to
-- customer_keys. customer_keys itself belongs to 000001.
DROP TABLE IF EXISTS customer_key_versions;
ALTER TABLE IF EXISTS customer_keys DROP CONSTRAINT IF EXISTS customer_keys_version_matches_rotations;
ALTER TABLE IF EXISTS customer_keys DROP CONSTRAINT IF EXISTS customer_keys_external_ref_required;
ALTER TABLE IF EXISTS customer_keys DROP CONSTRAINT IF EXISTS customer_keys_tenant_id_id;
