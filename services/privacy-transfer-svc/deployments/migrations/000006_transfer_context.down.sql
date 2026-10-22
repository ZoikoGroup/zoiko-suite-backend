-- 000006_transfer_context.down.sql

ALTER TABLE transfer_decisions
    DROP COLUMN IF EXISTS exporter_role,
    DROP COLUMN IF EXISTS importer_role,
    DROP COLUMN IF EXISTS origin_jurisdiction,
    DROP COLUMN IF EXISTS route_provider,
    DROP COLUMN IF EXISTS frequency_volume_class;
