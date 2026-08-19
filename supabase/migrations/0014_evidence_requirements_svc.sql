-- 0014_evidence_requirements_svc.sql
-- evidence-requirements-svc → schema `evidence_requirements`
--
-- End state of 000001_initial_schema (the service's only migration).
-- Two tables: evidence_requirements, evidence_evaluations.

CREATE SCHEMA IF NOT EXISTS evidence_requirements;

COMMENT ON SCHEMA evidence_requirements IS
    'evidence-requirements-svc. Evidence preconditions per (entity, domain, action) and the append-only ledger of gate determinations.';

GRANT USAGE ON SCHEMA evidence_requirements TO zoiko_backend, authenticated;

-- ── evidence_requirements ────────────────────────────────────────────────────

CREATE TABLE evidence_requirements.evidence_requirements (
    evidence_requirement_id UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID         NOT NULL,

    -- NULLABLE, and the NULL is meaningful: the requirement applies
    -- tenant-wide. A non-NULL value scopes it to one legal entity, which is
    -- what an entity-specific regulatory precondition needs.
    legal_entity_id         UUID,

    domain_code             VARCHAR(64)  NOT NULL,
    action_type             VARCHAR(64)  NOT NULL,
    evidence_type           VARCHAR(64)  NOT NULL,

    -- Sufficiency parameters as DATA (minimum_count, artifact_subtype,
    -- description). This column is what keeps the doctrine rule "no service may
    -- hardcode a country, jurisdiction, currency or tax-rule value as a code
    -- constant, enum or switch/case branch" satisfiable: adding a jurisdiction
    -- that demands a notarised signature is an INSERT, never a redeploy.
    requirement_payload     JSONB        NOT NULL DEFAULT '{}',

    -- Effective dating is the retirement mechanism. There is no is_deleted flag
    -- and no DELETE path.
    effective_from          TIMESTAMPTZ  NOT NULL,
    effective_to            TIMESTAMPTZ,

    -- Provenance: a governance rule whose author is unknown is not useful
    -- evidence.
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL DEFAULT app.current_principal_id(),
    retired_by_principal_id VARCHAR(255),
    retired_reason          TEXT,

    correlation_id          VARCHAR(255) NOT NULL,

    CONSTRAINT evidence_requirements_period_ordered
        CHECK (effective_to IS NULL OR effective_to > effective_from),

    -- A retired requirement must say who retired it. Retirement IS the
    -- end-dating, so the two travel together.
    CONSTRAINT evidence_requirements_retired_has_actor
        CHECK (effective_to IS NULL OR retired_by_principal_id IS NOT NULL)
);

CREATE INDEX idx_evidence_requirements_tenant
    ON evidence_requirements.evidence_requirements (tenant_id);
CREATE INDEX idx_evidence_requirements_entity
    ON evidence_requirements.evidence_requirements (legal_entity_id);

-- The evaluator's hot path: every requirement in force for one
-- (tenant, domain, action) at a point in time.
CREATE INDEX idx_evidence_requirements_lookup
    ON evidence_requirements.evidence_requirements
       (tenant_id, domain_code, action_type, effective_from, effective_to);

-- Idempotency: a retried create returns the original requirement rather than
-- minting a second. A real DB-level constraint, not a convention — most
-- services on this platform store correlation_id without constraining it, so
-- their retries silently duplicate.
CREATE UNIQUE INDEX idx_evidence_requirements_tenant_correlation
    ON evidence_requirements.evidence_requirements (tenant_id, correlation_id);

-- Natural key: one requirement per (entity scope, domain, action, evidence
-- type) per effective_from.
--
-- COALESCE is load-bearing. Postgres treats NULLs as distinct in a unique
-- index, so naming legal_entity_id directly would allow unlimited duplicate
-- tenant-wide rows for the same triple — exactly what this index exists to
-- prevent. Folding NULL onto the zero UUID makes tenant-wide rows compare
-- equal to each other.
CREATE UNIQUE INDEX idx_evidence_requirements_natural_key
    ON evidence_requirements.evidence_requirements (
        tenant_id,
        COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
        domain_code,
        action_type,
        evidence_type,
        effective_from
    );

-- ── evidence_evaluations ─────────────────────────────────────────────────────
-- Append-only determination ledger. This table is why the service is evidential
-- about ITSELF rather than only enforcing evidence on others: every gate
-- decision is recorded with the requirements that were unmet and the artifacts
-- the caller asserted, frozen at decision time so the record stays truthful
-- after the catalogue changes underneath it.

CREATE TABLE evidence_requirements.evidence_evaluations (
    evaluation_id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID         NOT NULL,
    legal_entity_id            UUID         NOT NULL,
    domain_code                VARCHAR(64)  NOT NULL,
    action_type                VARCHAR(64)  NOT NULL,

    -- SATISFIED | MISSING | NO_REQUIREMENTS_DEFINED. The third value is
    -- deliberate: an empty catalogue is a legitimate data state and must not be
    -- recorded as SATISFIED, which would make "nobody configured this yet"
    -- indistinguishable from "verified complete".
    outcome                    VARCHAR(32)  NOT NULL,

    unmet_payload              JSONB        NOT NULL DEFAULT '[]',
    present_artifacts_payload  JSONB        NOT NULL DEFAULT '[]',

    evaluated_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    evaluated_for_principal_id VARCHAR(255) NOT NULL,
    correlation_id             VARCHAR(255) NOT NULL,

    -- The gate used to fail OPEN on anything it did not recognise; naming the
    -- vocabulary keeps an unknown outcome from being stored and later read as
    -- a pass.
    CONSTRAINT evidence_evaluations_outcome_known
        CHECK (outcome IN ('SATISFIED', 'MISSING', 'NO_REQUIREMENTS_DEFINED')),

    -- A MISSING determination must say what was missing, or it is not a
    -- determination.
    CONSTRAINT evidence_evaluations_missing_names_unmet
        CHECK (outcome <> 'MISSING' OR jsonb_array_length(unmet_payload) > 0)
);

CREATE INDEX idx_evidence_evaluations_tenant
    ON evidence_requirements.evidence_evaluations (tenant_id);
CREATE INDEX idx_evidence_evaluations_action
    ON evidence_requirements.evidence_evaluations
       (tenant_id, domain_code, action_type, evaluated_at DESC);
CREATE INDEX idx_evidence_evaluations_outcome
    ON evidence_requirements.evidence_evaluations (outcome);

-- Replay safety: an evaluation retried with the same correlation_id returns the
-- original determination and does not republish its event.
CREATE UNIQUE INDEX idx_evidence_evaluations_tenant_correlation
    ON evidence_requirements.evidence_evaluations (tenant_id, correlation_id);

-- A determination is evidence of what was decided at a moment. Mutating one
-- rewrites the answer a gate gave, so this carries the trigger as well as the
-- withheld grant.
CREATE TRIGGER evidence_evaluations_immutable
    BEFORE UPDATE OR DELETE ON evidence_requirements.evidence_evaluations
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE evidence_requirements.evidence_requirements ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_requirements.evidence_requirements FORCE  ROW LEVEL SECURITY;
ALTER TABLE evidence_requirements.evidence_evaluations  ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_requirements.evidence_evaluations  FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON evidence_requirements.evidence_requirements
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON evidence_requirements.evidence_requirements
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_isolation ON evidence_requirements.evidence_evaluations
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

CREATE POLICY tenant_read ON evidence_requirements.evidence_evaluations
    FOR SELECT
    TO authenticated
    USING (tenant_id::text = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON evidence_requirements.evidence_requirements TO authenticated;
GRANT SELECT ON evidence_requirements.evidence_evaluations  TO authenticated;

-- UPDATE on requirements is the retirement path (setting effective_to and
-- retired_by); there is no DELETE.
GRANT SELECT, INSERT, UPDATE ON evidence_requirements.evidence_requirements TO zoiko_backend;
GRANT SELECT, INSERT         ON evidence_requirements.evidence_evaluations  TO zoiko_backend;
