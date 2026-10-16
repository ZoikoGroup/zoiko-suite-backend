-- 000006_transfer_context.up.sql
-- Complete section 16.1's TransferContext on the append-only transfer decision
-- evidence: exporter/importer roles, origin jurisdiction, route/provider
-- and frequency/volume class.
--
-- These are DATA ONLY (the spec names no taxonomy or enum for them) and
-- are nullable - existing decisions and callers that omit them keep
-- working. TEXT (not VARCHAR(16)) is deliberate: unlike the closed
-- destination_jurisdiction enumeration used by the workbench, the spec
-- gives no length-bound vocabulary here, so the column must not turn a
-- valid caller declaration into an insert-time failure.

ALTER TABLE transfer_decisions
    ADD COLUMN IF NOT EXISTS exporter_role          TEXT,
    ADD COLUMN IF NOT EXISTS importer_role          TEXT,
    ADD COLUMN IF NOT EXISTS origin_jurisdiction    TEXT,
    ADD COLUMN IF NOT EXISTS route_provider         TEXT,
    ADD COLUMN IF NOT EXISTS frequency_volume_class TEXT;
