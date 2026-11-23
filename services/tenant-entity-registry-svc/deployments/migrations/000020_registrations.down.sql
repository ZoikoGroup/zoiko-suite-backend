-- 000020_registrations.down.sql
-- Registrations are evidence other domains rely on. Refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM registrations) THEN
        RAISE EXCEPTION '000020 down refused: registrations exist and are evidence of record';
    END IF;
END
$$;

DROP TABLE IF EXISTS registration_history;
DROP TABLE IF EXISTS registrations;
DROP FUNCTION IF EXISTS registration_history_guard();
DROP FUNCTION IF EXISTS registration_guard();
DROP FUNCTION IF EXISTS registration_holder_tenant_guard();
