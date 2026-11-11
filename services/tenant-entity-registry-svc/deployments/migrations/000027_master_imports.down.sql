-- 000027_master_imports.down.sql
-- Imports are the evidence of how master records arrived. Refused while any import exists.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM master_imports) THEN
        RAISE EXCEPTION '000027 down refused: master imports exist and are evidence of how master data arrived';
    END IF;
END
$$;

DROP TABLE IF EXISTS master_import_rows;
DROP TABLE IF EXISTS master_imports;
DROP TABLE IF EXISTS async_operations;
DROP FUNCTION IF EXISTS master_import_row_guard();
DROP FUNCTION IF EXISTS master_import_guard();
