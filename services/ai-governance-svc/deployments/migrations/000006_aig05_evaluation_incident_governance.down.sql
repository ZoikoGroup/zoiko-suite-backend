DROP TRIGGER IF EXISTS trigger_incident_lifecycle ON ai_incidents;
DROP FUNCTION IF EXISTS aig05_enforce_incident_lifecycle() CASCADE;
DROP TABLE IF EXISTS ai_incidents;

DROP TRIGGER IF EXISTS trigger_evaluation_immutability ON ai_evaluations;
DROP FUNCTION IF EXISTS aig05_reject_evaluation_mutation() CASCADE;
DROP TABLE IF EXISTS ai_evaluations;

-- Restore 000004's original release lifecycle (QUARANTINED terminal
-- except for RETIRED; RESTRICTED cannot reach QUARANTINED directly).
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
                IF NEW.release_state NOT IN ('ACTIVE', 'RETIRED') THEN
                    RAISE EXCEPTION 'invalid ai_model_release transition from RESTRICTED to %', NEW.release_state;
                END IF;
            WHEN 'QUARANTINED' THEN
                IF NEW.release_state <> 'RETIRED' THEN
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
