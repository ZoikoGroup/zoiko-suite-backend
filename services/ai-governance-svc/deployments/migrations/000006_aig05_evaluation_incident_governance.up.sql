-- 000006_aig05_evaluation_incident_governance.up.sql
-- ZS-SVC-X-001 §8 — AIG-05: Evaluation, Monitoring, Incident & Change
-- Governance Service.
--
-- Additive alongside AIG-01/02/04 (000003/000004/000005), which are
-- untouched except for one deliberate extension: migration 000004's
-- own comment said "reactivation from quarantine needs AIG-05's
-- reactivation gate, a later wave, so it is deliberately not modeled
-- here yet" — this is that wave. The trigger function is replaced
-- (CREATE OR REPLACE, same name) to add exactly one new edge:
-- QUARANTINED -> ACTIVE, gated at the application layer by
-- ReactivateRelease (root cause documented + a passing re-evaluation
-- recorded after the incident).
--
-- ai_evaluations and ai_incidents are both caller-attested evidence —
-- no real eval-running pipeline or production telemetry exists in
-- this codebase to generate them automatically (same honest-scoping
-- already used for AIG-02's gate booleans and AIG-04's execution_ref),
-- so an external evaluator/operator submits results and this service
-- owns the governance consequences (release-state transitions,
-- incident lifecycle), not the measurement itself.

-- ── Reactivation: extend AIG-02's release lifecycle ─────────────────────────

CREATE OR REPLACE FUNCTION aig02_enforce_release_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ai_model_releases rows cannot be deleted';
    END IF;
    IF OLD.release_state IN ('REJECTED', 'BLOCKED', 'RETIRED') THEN
        RAISE EXCEPTION 'ai_model_release % is % and immutable', OLD.model_release_id, OLD.release_state;
    END IF;
    IF OLD.release_state <> NEW.release_state THEN
        CASE OLD.release_state
            WHEN 'DISCOVERED' THEN
                IF NEW.release_state NOT IN ('DUE_DILIGENCE', 'REJECTED') THEN
                    RAISE EXCEPTION 'invalid ai_model_release transition from DISCOVERED to %', NEW.release_state;
                END IF;
            WHEN 'DUE_DILIGENCE' THEN
                IF NEW.release_state NOT IN ('EVALUATING', 'REJECTED') THEN
                    RAISE EXCEPTION 'invalid ai_model_release transition from DUE_DILIGENCE to %', NEW.release_state;
                END IF;
            WHEN 'EVALUATING' THEN
                IF NEW.release_state NOT IN ('APPROVED', 'BLOCKED') THEN
                    RAISE EXCEPTION 'invalid ai_model_release transition from EVALUATING to %', NEW.release_state;
                END IF;
            WHEN 'APPROVED' THEN
                IF NEW.release_state <> 'ACTIVE' THEN
                    RAISE EXCEPTION 'invalid ai_model_release transition from APPROVED to %', NEW.release_state;
                END IF;
            WHEN 'ACTIVE' THEN
                IF NEW.release_state NOT IN ('RESTRICTED', 'QUARANTINED', 'RETIRED') THEN
                    RAISE EXCEPTION 'invalid ai_model_release transition from ACTIVE to %', NEW.release_state;
                END IF;
            WHEN 'RESTRICTED' THEN
                IF NEW.release_state NOT IN ('ACTIVE', 'QUARANTINED', 'RETIRED') THEN
                    RAISE EXCEPTION 'invalid ai_model_release transition from RESTRICTED to %', NEW.release_state;
                END IF;
            WHEN 'QUARANTINED' THEN
                -- AIG-05's reactivation gate: QUARANTINED -> ACTIVE is now
                -- reachable, but only ReactivateRelease issues it, and only
                -- after validating root-cause + re-evaluation evidence.
                IF NEW.release_state NOT IN ('ACTIVE', 'RETIRED') THEN
                    RAISE EXCEPTION 'invalid ai_model_release transition from QUARANTINED to %', NEW.release_state;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown ai_model_release release_state %', OLD.release_state;
        END CASE;
    END IF;
    IF NEW.provider IS DISTINCT FROM OLD.provider
        OR NEW.provider_model_id IS DISTINCT FROM OLD.provider_model_id
        OR NEW.deployment_region IS DISTINCT FROM OLD.deployment_region
        OR NEW.capability_set IS DISTINCT FROM OLD.capability_set
        OR NEW.context_limit IS DISTINCT FROM OLD.context_limit
        OR NEW.training_use IS DISTINCT FROM OLD.training_use
        OR NEW.retention IS DISTINCT FROM OLD.retention
        OR NEW.approved_scopes IS DISTINCT FROM OLD.approved_scopes
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'ai_model_release % identity fields are immutable; a changed model requires a new release', OLD.model_release_id;
    END IF;
    NEW.updated_at := NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── AI Evaluations ───────────────────────────────────────────────────────────

-- §8's 8 evaluation dimensions. One row per dimension per evaluation
-- run — a release typically accumulates several as its mandatory
-- suites are run, never one row trying to hold all 8 results.
CREATE TABLE ai_evaluations (
    evaluation_id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_release_id         UUID NOT NULL REFERENCES ai_model_releases(model_release_id),
    use_case_id              UUID REFERENCES ai_use_cases(use_case_id),
    dimension                VARCHAR(32) NOT NULL
        CHECK (dimension IN ('TASK_QUALITY', 'SAFETY', 'DOMAIN_INTEGRITY', 'FAIRNESS_IMPACT',
            'ROBUSTNESS', 'PRIVACY_SECURITY', 'OPERATIONS', 'HUMAN_FACTORS')),
    dataset_version           VARCHAR(128) NOT NULL,
    metrics                      JSONB NOT NULL DEFAULT '{}'::jsonb,
    thresholds                      JSONB NOT NULL DEFAULT '{}'::jsonb,
    result                              VARCHAR(8) NOT NULL CHECK (result IN ('PASS', 'FAIL')),
    defects                                JSONB NOT NULL DEFAULT '[]'::jsonb,
    evaluated_by_principal_id                 VARCHAR(255) NOT NULL,
    evaluated_at                                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_ai_evaluations_release ON ai_evaluations (model_release_id, dimension, evaluated_at DESC);

-- Immutable release evidence (§8's own words) — append-only, no
-- exceptions.
CREATE OR REPLACE FUNCTION aig05_reject_evaluation_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'ai_evaluations rows are immutable and never deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_evaluation_immutability
    BEFORE UPDATE OR DELETE ON ai_evaluations
    FOR EACH ROW EXECUTE FUNCTION aig05_reject_evaluation_mutation();

-- ── AI Incidents ──────────────────────────────────────────────────────────────

-- exception_case_ref is a logical (non-FK) reference to an
-- exception-escalation-svc ExceptionCase — services in this platform
-- do not share a database, so cross-service links are always
-- caller-attested identifiers, never real foreign keys. At least one
-- of model_release_id/use_case_id must be named: an incident with
-- nothing affected isn't an AI incident.
CREATE TABLE ai_incidents (
    incident_id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    severity                  VARCHAR(8) NOT NULL CHECK (severity IN ('AI-P0', 'AI-P1', 'AI-P2', 'AI-P3')),
    model_release_id          UUID REFERENCES ai_model_releases(model_release_id),
    use_case_id               UUID REFERENCES ai_use_cases(use_case_id),
    exception_case_ref        VARCHAR(255),
    incident_state            VARCHAR(16) NOT NULL DEFAULT 'OPEN'
        CHECK (incident_state IN ('OPEN', 'CONTAINED', 'RESOLVED', 'CLOSED')),
    description                TEXT NOT NULL CHECK (description <> ''),
    containment_action           TEXT,
    root_cause                       TEXT,
    corrective_actions                  TEXT,
    closure_evidence                       TEXT,
    reported_by_principal_id                  VARCHAR(255) NOT NULL,
    reported_at                                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    contained_at                                    TIMESTAMPTZ,
    resolved_at                                        TIMESTAMPTZ,
    closed_at                                             TIMESTAMPTZ,
    closed_by_principal_id                                   VARCHAR(255),
    CHECK (model_release_id IS NOT NULL OR use_case_id IS NOT NULL),
    CHECK (incident_state NOT IN ('CONTAINED', 'RESOLVED', 'CLOSED') OR containment_action IS NOT NULL),
    CHECK (incident_state NOT IN ('RESOLVED', 'CLOSED') OR (root_cause IS NOT NULL AND corrective_actions IS NOT NULL)),
    CHECK (incident_state <> 'CLOSED' OR closure_evidence IS NOT NULL)
);

CREATE INDEX idx_ai_incidents_release ON ai_incidents (model_release_id, incident_state);
CREATE INDEX idx_ai_incidents_severity ON ai_incidents (severity, incident_state);

-- Forward-only per §8.4/§8.6: OPEN -> CONTAINED -> RESOLVED -> CLOSED.
-- CLOSED is terminal. Each stage's required evidence field (checked
-- above) must be present before the transition into it — a CONTAINED
-- incident with no containment_action, or a RESOLVED one with no
-- root_cause, is rejected at the database, not just by convention.
CREATE OR REPLACE FUNCTION aig05_enforce_incident_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ai_incidents rows cannot be deleted';
    END IF;
    IF OLD.incident_state = 'CLOSED' THEN
        RAISE EXCEPTION 'ai_incident % is CLOSED and immutable', OLD.incident_id;
    END IF;
    IF OLD.incident_state <> NEW.incident_state THEN
        CASE OLD.incident_state
            WHEN 'OPEN' THEN
                IF NEW.incident_state <> 'CONTAINED' THEN
                    RAISE EXCEPTION 'invalid ai_incident transition from OPEN to %', NEW.incident_state;
                END IF;
            WHEN 'CONTAINED' THEN
                IF NEW.incident_state <> 'RESOLVED' THEN
                    RAISE EXCEPTION 'invalid ai_incident transition from CONTAINED to %', NEW.incident_state;
                END IF;
            WHEN 'RESOLVED' THEN
                IF NEW.incident_state <> 'CLOSED' THEN
                    RAISE EXCEPTION 'invalid ai_incident transition from RESOLVED to %', NEW.incident_state;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown ai_incident incident_state %', OLD.incident_state;
        END CASE;
    END IF;
    IF NEW.severity IS DISTINCT FROM OLD.severity
        OR NEW.model_release_id IS DISTINCT FROM OLD.model_release_id
        OR NEW.use_case_id IS DISTINCT FROM OLD.use_case_id
        OR NEW.description IS DISTINCT FROM OLD.description
        OR NEW.reported_at IS DISTINCT FROM OLD.reported_at
        OR NEW.reported_by_principal_id IS DISTINCT FROM OLD.reported_by_principal_id
    THEN
        RAISE EXCEPTION 'ai_incident % identity fields are immutable', OLD.incident_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_incident_lifecycle
    BEFORE UPDATE OR DELETE ON ai_incidents
    FOR EACH ROW EXECUTE FUNCTION aig05_enforce_incident_lifecycle();
