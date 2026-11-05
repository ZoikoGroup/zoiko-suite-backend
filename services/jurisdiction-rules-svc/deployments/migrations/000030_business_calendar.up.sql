-- 000030_business_calendar.up.sql
--
-- REF-10 Business Calendar & Payment Terms (ZS-SVC-I-001 s4.20), the calendar
-- half: "holidays/business-day conventions", "observed-holiday rules", "Source
-- holiday authority", "holiday override versioned", "business-day calculation
-- deterministic".
--
-- The regulatory calendar tables of 000010 already hold calendars, immutable
-- effective-dated versions, weekend definitions and holidays, published by a
-- different principal. REF-10 extends them rather than adding a second calendar:
--
--   * a holiday can carry an OBSERVED-HOLIDAY rule, so a holiday on a weekend is
--     observed on a weekday, and business-day arithmetic uses the observed date;
--   * a version can carry the evidence of the IMPORT it came from (the holiday
--     authority, its reference, its source version, and a hash of the content),
--     and one source version is imported once per calendar.
--
-- Both are set when the version is drafted and frozen by the existing guards
-- when it is published, so an amended calendar is a new version and earlier
-- results keep the version they recorded.

ALTER TABLE regulatory_holidays
    ADD COLUMN IF NOT EXISTS observed_rule VARCHAR(32) NOT NULL DEFAULT 'NONE',
    ADD COLUMN IF NOT EXISTS observed_date DATE;

ALTER TABLE regulatory_holidays DROP CONSTRAINT IF EXISTS ck_holiday_observed_rule;
ALTER TABLE regulatory_holidays
    ADD CONSTRAINT ck_holiday_observed_rule CHECK (observed_rule IN (
        'NONE', 'WEEKEND_TO_NEXT_WEEKDAY', 'WEEKEND_TO_PREVIOUS_WEEKDAY', 'WEEKEND_TO_NEAREST_WEEKDAY'));

-- An observed date exists exactly when there is a rule to observe by.
ALTER TABLE regulatory_holidays DROP CONSTRAINT IF EXISTS ck_holiday_observed_shape;
ALTER TABLE regulatory_holidays
    ADD CONSTRAINT ck_holiday_observed_shape CHECK ((observed_rule = 'NONE') = (observed_date IS NULL));

ALTER TABLE regulatory_calendar_versions
    ADD COLUMN IF NOT EXISTS source_authority TEXT,
    ADD COLUMN IF NOT EXISTS source_reference TEXT,
    ADD COLUMN IF NOT EXISTS source_version   VARCHAR(64),
    ADD COLUMN IF NOT EXISTS import_hash      CHAR(64);

-- A version is either hand-authored (no import evidence) or imported with all of it.
ALTER TABLE regulatory_calendar_versions DROP CONSTRAINT IF EXISTS ck_calendar_import_complete;
ALTER TABLE regulatory_calendar_versions
    ADD CONSTRAINT ck_calendar_import_complete CHECK (
        (source_authority IS NULL AND source_reference IS NULL AND source_version IS NULL AND import_hash IS NULL)
        OR (btrim(COALESCE(source_authority, '')) <> '' AND btrim(COALESCE(source_reference, '')) <> ''
            AND btrim(COALESCE(source_version, '')) <> '' AND import_hash ~ '^[0-9a-f]{64}$'));

-- "Calendar/version ... scoped by ... effective interval": one source version is
-- imported once per calendar, so a repeat is a replay and never a second version.
CREATE UNIQUE INDEX IF NOT EXISTS uq_calendar_import_source_version
    ON regulatory_calendar_versions (calendar_id, source_version)
    WHERE source_version IS NOT NULL;

-- The evidence of an import is never rewritten, even on a draft.
CREATE OR REPLACE FUNCTION jur_calendar_version_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'PUBLISHED' THEN
        RAISE EXCEPTION 'a published calendar version is immutable; publish a new version' USING ERRCODE = '23514';
    END IF;
    IF NEW.calendar_version_id <> OLD.calendar_version_id OR NEW.calendar_id <> OLD.calendar_id OR NEW.version <> OLD.version
       OR NEW.created_at <> OLD.created_at OR NEW.created_by_principal_id <> OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'calendar version identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.source_authority IS DISTINCT FROM OLD.source_authority OR NEW.source_reference IS DISTINCT FROM OLD.source_reference
       OR NEW.source_version IS DISTINCT FROM OLD.source_version OR NEW.import_hash IS DISTINCT FROM OLD.import_hash THEN
        RAISE EXCEPTION 'the evidence of a calendar import is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'PUBLISHED' AND NOT EXISTS (SELECT 1 FROM calendar_version_sources WHERE calendar_version_id = OLD.calendar_version_id) THEN
        RAISE EXCEPTION 'a calendar version cannot be published without a source' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
