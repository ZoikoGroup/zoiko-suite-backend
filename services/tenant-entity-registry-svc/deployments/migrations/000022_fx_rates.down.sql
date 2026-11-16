-- 000022_fx_rates.down.sql
-- Exchange rates are evidence that accounting, tax and reporting rely on. Refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM fx_sources) OR EXISTS (SELECT 1 FROM fx_observations) THEN
        RAISE EXCEPTION '000022 down refused: exchange-rate sources or observations exist';
    END IF;
END
$$;

DROP TABLE IF EXISTS fx_rate_set_members;
DROP TABLE IF EXISTS fx_rate_sets;
DROP TABLE IF EXISTS fx_observations;
DROP TABLE IF EXISTS fx_sources;
DROP FUNCTION IF EXISTS fx_member_guard();
DROP FUNCTION IF EXISTS fx_rate_set_guard();
DROP FUNCTION IF EXISTS fx_observation_guard();
DROP FUNCTION IF EXISTS fx_source_guard();
