ALTER TABLE fixed_assets DROP CONSTRAINT chk_fixed_asset_status;
ALTER TABLE fixed_assets ADD CONSTRAINT chk_fixed_asset_status
    CHECK (status IN ('CANDIDATE', 'REGISTERED', 'ACTIVE', 'SUSPENDED', 'MERGED'));

DROP TABLE IF EXISTS asset_events;
DROP FUNCTION IF EXISTS reject_asset_event_economic_mutation();
