-- ORG-01 wave 1: assurance history.
--
-- Until now the assurance a session was established with lived on auth_sessions.assurance_level and in the
-- token, and a step-up left no record beyond a decision-log line. GetAssuranceHistory needs every change kept,
-- so each is written here as an observation. Observations are captured by triggers, not by the application
-- paths that cause them: a login, a federated login, a step-up and any future path are all covered, and a
-- path added later cannot forget to record one.
--
-- Evidence holds identifiers, methods and levels. Never a code, a token or a secret.

CREATE OR REPLACE FUNCTION assurance_rank(level TEXT) RETURNS INT AS $$
    SELECT CASE level WHEN 'SINGLE_FACTOR' THEN 1 WHEN 'MFA' THEN 2 WHEN 'PHISHING_RESISTANT' THEN 3 ELSE 0 END
$$ LANGUAGE sql IMMUTABLE;

CREATE TABLE assurance_observations (
    observation_id   VARCHAR(64)  PRIMARY KEY DEFAULT gen_random_uuid()::text,
    tenant_id        VARCHAR(255) NOT NULL,
    principal_id     VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    session_id       VARCHAR(64),
    observed_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    assurance_level  VARCHAR(32)  NOT NULL,
    previous_level   VARCHAR(32),
    change_kind      VARCHAR(16)  NOT NULL,
    auth_methods     TEXT[]       NOT NULL,
    idp_id           VARCHAR(255),
    correlation_id   VARCHAR(255) NOT NULL DEFAULT '',
    CONSTRAINT assurance_obs_level CHECK (assurance_level IN ('SINGLE_FACTOR', 'MFA', 'PHISHING_RESISTANT')),
    CONSTRAINT assurance_obs_prev CHECK (previous_level IS NULL OR previous_level IN ('SINGLE_FACTOR', 'MFA', 'PHISHING_RESISTANT')),
    CONSTRAINT assurance_obs_kind CHECK (change_kind IN ('ESTABLISHED', 'STEP_UP', 'DOWNGRADE', 'METHODS_CHANGED')),
    -- A change has something to change from; only the first observation of a session has no previous level.
    CONSTRAINT assurance_obs_prev_pair CHECK ((change_kind = 'ESTABLISHED') = (previous_level IS NULL) OR change_kind = 'STEP_UP')
);
CREATE INDEX idx_assurance_obs_principal ON assurance_observations (tenant_id, principal_id, observed_at DESC, observation_id);

ALTER TABLE assurance_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE assurance_observations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON assurance_observations FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION assurance_observations_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'assurance observations are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER assurance_observations_guard BEFORE UPDATE OR DELETE ON assurance_observations
    FOR EACH ROW EXECUTE FUNCTION assurance_observations_guard();

-- A login establishes assurance for its session.
CREATE OR REPLACE FUNCTION auth_session_assurance_insert() RETURNS trigger AS $$
BEGIN
    INSERT INTO assurance_observations (tenant_id, principal_id, session_id, observed_at, assurance_level,
                                        previous_level, change_kind, auth_methods, idp_id)
    VALUES (NEW.tenant_id, NEW.principal_id, NEW.sid, NEW.created_at, NEW.assurance_level,
            NULL, 'ESTABLISHED', NEW.amr, NEW.idp_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER auth_session_assurance_insert AFTER INSERT ON auth_sessions
    FOR EACH ROW EXECUTE FUNCTION auth_session_assurance_insert();

-- A session whose level or methods change records the change.
CREATE OR REPLACE FUNCTION auth_session_assurance_update() RETURNS trigger AS $$
DECLARE
    kind TEXT;
BEGIN
    IF NEW.assurance_level = OLD.assurance_level AND NEW.amr IS NOT DISTINCT FROM OLD.amr THEN
        RETURN NEW;
    END IF;
    IF assurance_rank(NEW.assurance_level) > assurance_rank(OLD.assurance_level) THEN
        kind := 'STEP_UP';
    ELSIF assurance_rank(NEW.assurance_level) < assurance_rank(OLD.assurance_level) THEN
        kind := 'DOWNGRADE';
    ELSE
        kind := 'METHODS_CHANGED';
    END IF;
    INSERT INTO assurance_observations (tenant_id, principal_id, session_id, assurance_level,
                                        previous_level, change_kind, auth_methods, idp_id)
    VALUES (NEW.tenant_id, NEW.principal_id, NEW.sid, NEW.assurance_level,
            OLD.assurance_level, kind, NEW.amr, NEW.idp_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER auth_session_assurance_update AFTER UPDATE OF assurance_level, amr ON auth_sessions
    FOR EACH ROW EXECUTE FUNCTION auth_session_assurance_update();

-- A step-up mints a fresh token for the SAME session and changes no auth_sessions row, so the decision the
-- step-up writes is where it is seen. Only a verified second factor counts; a refused attempt raises nothing.
CREATE OR REPLACE FUNCTION step_up_assurance() RETURNS trigger AS $$
DECLARE
    prior TEXT;
BEGIN
    IF NEW.action_type <> 'MFA_STEP_UP' OR NEW.decision_outcome <> 'ALLOWED' THEN
        RETURN NEW;
    END IF;
    SELECT assurance_level INTO prior FROM assurance_observations
     WHERE tenant_id = NEW.tenant_id AND principal_id = NEW.principal_id
     ORDER BY observed_at DESC, observation_id DESC LIMIT 1;
    INSERT INTO assurance_observations (tenant_id, principal_id, observed_at, assurance_level, previous_level,
                                        change_kind, auth_methods, correlation_id)
    VALUES (NEW.tenant_id, NEW.principal_id, NEW.decided_at, 'MFA', prior, 'STEP_UP',
            ARRAY['pwd', 'otp'], NEW.correlation_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER step_up_assurance AFTER INSERT ON access_decision_log
    FOR EACH ROW EXECUTE FUNCTION step_up_assurance();

-- History before this migration: each existing login as its own established observation.
INSERT INTO assurance_observations (tenant_id, principal_id, session_id, observed_at, assurance_level,
                                    previous_level, change_kind, auth_methods, idp_id)
SELECT tenant_id, principal_id, sid, created_at, assurance_level, NULL, 'ESTABLISHED', amr, idp_id
  FROM auth_sessions;

-- The authentication-event read model reads the decision log by person and time.
CREATE INDEX IF NOT EXISTS idx_decision_log_principal_time
    ON access_decision_log (tenant_id, principal_id, decided_at DESC, decision_log_id);
CREATE INDEX IF NOT EXISTS idx_federation_login_log_principal
    ON federation_login_log (tenant_id, principal_id, created_at DESC) WHERE principal_id IS NOT NULL;
