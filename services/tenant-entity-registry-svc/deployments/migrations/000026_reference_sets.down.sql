-- 000026_reference_sets.down.sql
-- Releases are what consumers resolved codes against. Refused while any reference set exists.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reference_sets) THEN
        RAISE EXCEPTION '000026 down refused: reference sets exist and are referenced master data';
    END IF;
END
$$;

DROP TABLE IF EXISTS reference_entries;
DROP TABLE IF EXISTS reference_releases;
DROP TABLE IF EXISTS reference_sets;
DROP FUNCTION IF EXISTS reference_entry_guard();
DROP FUNCTION IF EXISTS reference_release_guard();
DROP FUNCTION IF EXISTS reference_set_guard();
