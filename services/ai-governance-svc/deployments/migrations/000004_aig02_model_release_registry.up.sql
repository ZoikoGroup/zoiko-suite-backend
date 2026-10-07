-- 000004_aig02_model_release_registry.up.sql
-- ZS-SVC-X-001 §5 — AIG-02: Model, Provider & Capability Registry.
--
-- Additive alongside model_provider_registrations (000001), which is
-- untouched — that table is doc7 §G6's simpler, mutable-in-place
-- provider registry. ai_model_releases is a distinct, much deeper
-- concept: "models are deployable artifacts with changing behavior and
-- contractual conditions, not interchangeable strings" (§5 SECTION
-- CONTROL). Every release is immutable once registered and moves
-- through a governed, forward-only state machine — "provider alias
-- movement or silent behavior change" always produces a NEW release
-- row, never a mutation of an existing model_release_id (§5.2, §5.3).
--
-- Platform-wide, no tenant_id — same convention as
-- model_provider_registrations: a model release is approved once for
-- the platform, not re-approved per tenant.
CREATE TABLE ai_model_releases (
    model_release_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider                 VARCHAR(128) NOT NULL,
    provider_model_id            VARCHAR(255) NOT NULL,
    deployment_region                VARCHAR(64) NOT NULL,
    capability_set                      JSONB NOT NULL DEFAULT '[]'::jsonb,
    context_limit                          INT,
    training_use                              VARCHAR(32) NOT NULL DEFAULT 'NO_TRAINING'
        CHECK (training_use IN ('NO_TRAINING','OPT_OUT_AVAILABLE','ALLOWED')),
    retention                                    TEXT NOT NULL DEFAULT '',
    approved_scopes                                 JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- control_evidence captures §5.1's narrative sub-fields (provider
    -- identity, deployment topology, security, contractual posture,
    -- evaluation status, cost/limits, fallback compatibility) that don't
    -- need individual typed columns at this stage, plus the §5.4
    -- procurement/enablement gate attestations an operator records
    -- before approval — caller-supplied evidence, same honest-scoping
    -- pattern used for AI-01/02/04/05 elsewhere in this platform, since
    -- no live PDC/PRV/security-posture integration exists yet.
    control_evidence                                   JSONB NOT NULL DEFAULT '{}'::jsonb,
    release_state                                         VARCHAR(16) NOT NULL DEFAULT 'DISCOVERED'
        CHECK (release_state IN ('DISCOVERED','DUE_DILIGENCE','EVALUATING','APPROVED','ACTIVE','RESTRICTED','QUARANTINED','REJECTED','BLOCKED','RETIRED')),
    status_reason                                            TEXT,
    created_at                                                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id                                        VARCHAR(255) NOT NULL,
    updated_at                                                        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_ai_model_releases_lookup ON ai_model_releases (provider, provider_model_id, release_state);

-- No RLS: platform-wide, same reasoning as model_provider_registrations
-- in 000002 — adding a tenant column here would invent a boundary the
-- doc rules out.

-- ── Lifecycle trigger ────────────────────────────────────────────────────────

-- Forward-only per §5.2's release-state diagram. REJECTED, BLOCKED and
-- RETIRED are fully terminal. QUARANTINED may only reach RETIRED —
-- reactivation from quarantine needs AIG-05's reactivation gate, a
-- later wave, so it is deliberately not modeled here yet.
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

CREATE TRIGGER trigger_release_lifecycle
    BEFORE UPDATE OR DELETE ON ai_model_releases
    FOR EACH ROW EXECUTE FUNCTION aig02_enforce_release_lifecycle();
