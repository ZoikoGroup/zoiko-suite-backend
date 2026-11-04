-- 000030 down. The import evidence and the observed-holiday rules are part of what
-- a published calendar says; dropping them loses that, so it is refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM regulatory_calendar_versions WHERE import_hash IS NOT NULL)
       OR EXISTS (SELECT 1 FROM regulatory_holidays WHERE observed_rule <> 'NONE') THEN
        RAISE EXCEPTION '000030 down refused: imported calendar versions or observed-holiday rules exist and would be lost';
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION jur_calendar_version_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'PUBLISHED' THEN
        RAISE EXCEPTION 'a published calendar version is immutable; publish a new version' USING ERRCODE = '23514';
    END IF;
    IF NEW.calendar_version_id <> OLD.calendar_version_id OR NEW.calendar_id <> OLD.calendar_id OR NEW.version <> OLD.version
       OR NEW.created_at <> OLD.created_at OR NEW.created_by_principal_id <> OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'calendar version identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'PUBLISHED' AND NOT EXISTS (SELECT 1 FROM calendar_version_sources WHERE calendar_version_id = OLD.calendar_version_id) THEN
        RAISE EXCEPTION 'a calendar version cannot be published without a source' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP INDEX IF EXISTS uq_calendar_import_source_version;
ALTER TABLE regulatory_calendar_versions DROP CONSTRAINT IF EXISTS ck_calendar_import_complete;
ALTER TABLE regulatory_calendar_versions
    DROP COLUMN IF EXISTS import_hash,
    DROP COLUMN IF EXISTS source_version,
    DROP COLUMN IF EXISTS source_reference,
    DROP COLUMN IF EXISTS source_authority;

ALTER TABLE regulatory_holidays DROP CONSTRAINT IF EXISTS ck_holiday_observed_shape;
ALTER TABLE regulatory_holidays DROP CONSTRAINT IF EXISTS ck_holiday_observed_rule;
ALTER TABLE regulatory_holidays
    DROP COLUMN IF EXISTS observed_date,
    DROP COLUMN IF EXISTS observed_rule;
