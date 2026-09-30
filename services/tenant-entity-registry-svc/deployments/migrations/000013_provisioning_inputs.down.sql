-- 000013_provisioning_inputs.down.sql

ALTER TABLE tenants
    DROP COLUMN IF EXISTS subscription_id,
    DROP COLUMN IF EXISTS primary_jurisdiction_id;
