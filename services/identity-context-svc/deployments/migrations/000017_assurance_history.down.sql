-- Migration 000017 down. The observations are evidence of what assurance a session was given; dropping them
-- loses it, so it is refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM assurance_observations) THEN
        RAISE EXCEPTION '000017 down refused: assurance observations exist and are evidence';
    END IF;
END
$$;

DROP INDEX IF EXISTS idx_federation_login_log_principal;
DROP INDEX IF EXISTS idx_decision_log_principal_time;
DROP TRIGGER IF EXISTS step_up_assurance ON access_decision_log;
DROP FUNCTION IF EXISTS step_up_assurance();
DROP TRIGGER IF EXISTS auth_session_assurance_update ON auth_sessions;
DROP FUNCTION IF EXISTS auth_session_assurance_update();
DROP TRIGGER IF EXISTS auth_session_assurance_insert ON auth_sessions;
DROP FUNCTION IF EXISTS auth_session_assurance_insert();
DROP TABLE IF EXISTS assurance_observations;
DROP FUNCTION IF EXISTS assurance_observations_guard();
DROP FUNCTION IF EXISTS assurance_rank(TEXT);
