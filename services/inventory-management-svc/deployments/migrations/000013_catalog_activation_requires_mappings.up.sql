-- Migration: 000013_catalog_activation_requires_mappings.up.sql
--
-- BIZ-07 failure/degradation row: "Missing required mapping blocks use in
-- protected transaction." The handler already refuses to activate a version
-- without them (422 CATALOG_MAPPING_REQUIRED); this trigger makes the rule
-- hold against any writer, so a direct UPDATE cannot put a version into
-- ACTIVE without a TAX and an ACCOUNTING mapping.
--
-- Required set: TAX and ACCOUNTING (PRODUCT is optional). Keep in step with
-- domain.RequiredCatalogMappingTypes.
--
-- Fires only on the transition INTO 'ACTIVE'. Rows already ACTIVE are left
-- alone (no retroactive rewrite); later suspend/retire/supersede are unaffected.
CREATE OR REPLACE FUNCTION enforce_catalog_activation_mappings()
RETURNS TRIGGER AS $$
DECLARE
    missing TEXT;
BEGIN
    IF NEW.status = 'ACTIVE' AND OLD.status IS DISTINCT FROM 'ACTIVE' THEN
        SELECT string_agg(req.t, ', ' ORDER BY req.t) INTO missing
        FROM (VALUES ('ACCOUNTING'), ('TAX')) AS req(t)
        WHERE NOT EXISTS (
            SELECT 1 FROM inventory_catalog_mappings m
            WHERE m.version_id = NEW.version_id
              AND m.tenant_id = NEW.tenant_id
              AND m.mapping_type = req.t
        );
        IF missing IS NOT NULL THEN
            RAISE EXCEPTION 'CATALOG_MAPPING_REQUIRED: version % is missing required mapping(s): %', NEW.version_id, missing
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_enforce_catalog_activation_mappings
    BEFORE UPDATE ON inventory_catalog_offering_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_catalog_activation_mappings();
