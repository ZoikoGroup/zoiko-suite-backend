DROP TRIGGER IF EXISTS trg_currencies_codes_immutable ON currencies;
DROP TRIGGER IF EXISTS trg_currencies_no_delete ON currencies;
DROP TRIGGER IF EXISTS trg_currency_imports_append_only ON currency_imports;
DROP TRIGGER IF EXISTS trg_status_history_append_only ON currency_status_history;
DROP TRIGGER IF EXISTS trg_minor_unit_versions_append_only ON currency_minor_unit_versions;
DROP FUNCTION IF EXISTS currency_registry_codes_immutable();
DROP FUNCTION IF EXISTS currency_registry_forbid_delete();
DROP FUNCTION IF EXISTS currency_registry_forbid_mutation();
