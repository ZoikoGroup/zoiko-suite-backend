ALTER TABLE IF EXISTS currencies DROP CONSTRAINT IF EXISTS currencies_last_import_fk;
DROP TABLE IF EXISTS currency_status_history;
DROP TABLE IF EXISTS currency_minor_unit_versions;
DROP TABLE IF EXISTS currency_imports;
DROP TABLE IF EXISTS currencies;
