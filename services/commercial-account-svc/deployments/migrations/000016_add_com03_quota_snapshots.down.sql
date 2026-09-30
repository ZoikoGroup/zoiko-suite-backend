-- 000016_add_com03_quota_snapshots.down.sql
ALTER TABLE dunning_cases DROP COLUMN IF EXISTS applied_restriction_id;
DROP TABLE IF EXISTS entitlement_snapshots;
ALTER TABLE price_version_capabilities DROP CONSTRAINT IF EXISTS price_version_capabilities_meter_pair;
ALTER TABLE price_version_capabilities DROP COLUMN IF EXISTS meter_key;
ALTER TABLE price_version_capabilities DROP COLUMN IF EXISTS meter_version;
