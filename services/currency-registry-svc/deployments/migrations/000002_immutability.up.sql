-- Immutability: no hard delete on any material table, and the evidence tables
-- are strictly append-only. Enforced in the database so that neither a bug in
-- this service nor a fix-up script can rewrite history.

CREATE OR REPLACE FUNCTION currency_registry_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% on % is forbidden: this table is append-only (use a new version / status transition)',
        TG_OP, TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION currency_registry_forbid_delete() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'DELETE on % is forbidden: retire by status transition, never by deleting', TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

-- Append-only: minor-unit versions, status history, import evidence.
CREATE TRIGGER trg_minor_unit_versions_append_only
    BEFORE UPDATE OR DELETE ON currency_minor_unit_versions
    FOR EACH ROW EXECUTE FUNCTION currency_registry_forbid_mutation();
CREATE TRIGGER trg_status_history_append_only
    BEFORE UPDATE OR DELETE ON currency_status_history
    FOR EACH ROW EXECUTE FUNCTION currency_registry_forbid_mutation();
CREATE TRIGGER trg_currency_imports_append_only
    BEFORE UPDATE OR DELETE ON currency_imports
    FOR EACH ROW EXECUTE FUNCTION currency_registry_forbid_mutation();

-- Updatable (status/version) but never deletable.
CREATE TRIGGER trg_currencies_no_delete
    BEFORE DELETE ON currencies
    FOR EACH ROW EXECUTE FUNCTION currency_registry_forbid_delete();

-- Codes are identity-adjacent and immutable once registered; only name, flag,
-- status, validity end, version and SoD evidence may change. A retired currency
-- is terminal.
CREATE OR REPLACE FUNCTION currency_registry_codes_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.currency_id <> OLD.currency_id OR NEW.alpha_code <> OLD.alpha_code
       OR NEW.numeric_code <> OLD.numeric_code OR NEW.valid_from <> OLD.valid_from THEN
        RAISE EXCEPTION 'currency_id, alpha_code, numeric_code and valid_from are immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'a RETIRED currency can no longer change' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_currencies_codes_immutable
    BEFORE UPDATE ON currencies
    FOR EACH ROW EXECUTE FUNCTION currency_registry_codes_immutable();
