-- 000029 down. Import batches are the evidence of what an external source
-- changed and who let it through; dropping them loses that, so it is refused
-- while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM jurisdiction_import_batches) THEN
        RAISE EXCEPTION '000029 down refused: jurisdiction import batches exist and would be lost';
    END IF;
END
$$;

DROP TABLE IF EXISTS jurisdiction_import_items;
DROP TABLE IF EXISTS jurisdiction_import_batches;
DROP FUNCTION IF EXISTS jurisdiction_import_items_guard();
DROP FUNCTION IF EXISTS jurisdiction_import_batches_guard();
