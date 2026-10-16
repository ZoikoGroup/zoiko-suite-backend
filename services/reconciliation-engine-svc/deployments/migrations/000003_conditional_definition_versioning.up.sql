-- Migration: 000003_conditional_definition_versioning.up.sql
--
-- 000001's enforce_definition_versioning bumped version on EVERY update, so
-- re-posting a definition with identical rules produced a new version that
-- differed from the previous one in nothing. Runs and certifications pin
-- definition_version, so a version must mean "these rules". The trigger stays
-- the single owner of version/updated_at (any UPDATE path, not only the
-- DefineReconciliation upsert): it bumps both only when a rule column
-- changes, and otherwise keeps the stored values whatever the statement set.

CREATE OR REPLACE FUNCTION enforce_definition_versioning()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation definitions cannot be deleted';
    END IF;
    IF (NEW.match_key_fields, NEW.tolerance_amount_minor_units, NEW.materiality_amount_minor_units)
       IS DISTINCT FROM
       (OLD.match_key_fields, OLD.tolerance_amount_minor_units, OLD.materiality_amount_minor_units) THEN
        NEW.version := OLD.version + 1;
        NEW.updated_at := NOW();
    ELSE
        NEW.version := OLD.version;
        NEW.updated_at := OLD.updated_at;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
