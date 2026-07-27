-- Migration: 000001_initial_schema.up.sql
--
-- Owned records for evidence-requirements-svc per
-- docs/architecture/03-microservices.md §8.6: evidence preconditions,
-- document requirements, signature requirements, supporting artifact rules,
-- and evidence sufficiency logic. Entity shape follows
-- docs/architecture/04-data-model.md §7.1 (EvidenceRequirement); every column
-- beyond that list is marked below with why it is here.
--
-- RLS is defense-in-depth, not the sole isolation guarantee — this platform's
-- services connect as a Postgres superuser, which unconditionally bypasses
-- RLS. Every query in this service's store layer filters explicitly by
-- tenant_id in its own SQL for that reason.

CREATE TABLE evidence_requirements (
    evidence_requirement_id  UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL,

    -- NOT in 04-data-model.md §7.1's attribute list. Added because
    -- .agents/rules/doctrine.md requires every material record to carry
    -- legal_entity_id, and the two sources conflict (context.md §11.1).
    -- NULLABLE, and NULL is meaningful: the requirement applies tenant-wide.
    -- A non-NULL value scopes it to one legal entity, which is what an
    -- entity-specific regulatory precondition needs.
    legal_entity_id          UUID,

    domain_code              VARCHAR(64) NOT NULL,
    action_type              VARCHAR(64) NOT NULL,
    evidence_type            VARCHAR(64) NOT NULL,

    -- Sufficiency parameters as DATA (minimum_count, artifact_subtype,
    -- description) — see domain.RequirementSpec. This column is what keeps
    -- the doctrine rule "no service may hardcode a country, jurisdiction,
    -- currency, or tax-rule value as a code constant, enum, or switch/case
    -- branch" satisfiable: adding a jurisdiction that demands a notarised
    -- signature is an INSERT, never a code change or redeploy.
    requirement_payload      JSONB NOT NULL DEFAULT '{}',

    -- Effective dating is the retirement mechanism. There is no is_deleted
    -- flag and no DELETE path (doctrine: no soft-delete on material objects;
    -- status transitions, tombstones, or effective end-dating only).
    effective_from           TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to             TIMESTAMP WITH TIME ZONE,

    -- Provenance. Beyond §7.1's list, but a governance rule whose author is
    -- unknown is not useful evidence (§17.6 "Every Material Service Must Be
    -- Evidential").
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL,
    retired_by_principal_id  VARCHAR(255),
    retired_reason           TEXT,

    correlation_id           VARCHAR(255) NOT NULL
);

CREATE INDEX idx_evidence_requirements_tenant ON evidence_requirements (tenant_id);
CREATE INDEX idx_evidence_requirements_entity ON evidence_requirements (legal_entity_id);

-- The evaluator's hot path: resolve every requirement in force for one
-- (tenant, domain, action) at a point in time.
CREATE INDEX idx_evidence_requirements_lookup
    ON evidence_requirements (tenant_id, domain_code, action_type, effective_from, effective_to);

-- Idempotency: a retried create with the same (tenant_id, correlation_id)
-- must return the original requirement, never mint a second one. This is a
-- real DB-level constraint, not a convention — most services on this
-- platform store correlation_id without constraining it, so their retries
-- silently duplicate.
CREATE UNIQUE INDEX idx_evidence_requirements_tenant_correlation
    ON evidence_requirements (tenant_id, correlation_id);

-- Natural key: one requirement per (entity scope, domain, action, evidence
-- type) per effective_from.
--
-- COALESCE is load-bearing. Postgres treats NULLs as distinct in a unique
-- index, so listing legal_entity_id directly would allow unlimited duplicate
-- tenant-wide rows for the same triple — exactly the duplicates this index
-- exists to prevent. Folding NULL onto the zero UUID makes tenant-wide rows
-- compare equal to each other.
CREATE UNIQUE INDEX idx_evidence_requirements_natural_key
    ON evidence_requirements (
        tenant_id,
        COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
        domain_code,
        action_type,
        evidence_type,
        effective_from
    );

ALTER TABLE evidence_requirements ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_requirements
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

-- Append-only determination ledger — never UPDATEd, never DELETEd.
--
-- This table is why this service is evidential about itself rather than only
-- enforcing evidence on others: every gate decision it makes is recorded with
-- the requirements that were unmet and the artifacts the caller asserted,
-- frozen at decision time so the record stays truthful even after the catalog
-- changes underneath it.
CREATE TABLE evidence_evaluations (
    evaluation_id                UUID PRIMARY KEY,
    tenant_id                    UUID NOT NULL,
    legal_entity_id              UUID NOT NULL,
    domain_code                  VARCHAR(64) NOT NULL,
    action_type                  VARCHAR(64) NOT NULL,

    -- SATISFIED | MISSING | NO_REQUIREMENTS_DEFINED. The third value is
    -- deliberate: an empty catalog is a legitimate data state and must not be
    -- recorded as SATISFIED, which would make "nobody configured this yet"
    -- indistinguishable from "verified complete" (context.md §5.3).
    outcome                      VARCHAR(32) NOT NULL,

    unmet_payload                JSONB NOT NULL DEFAULT '[]',
    present_artifacts_payload    JSONB NOT NULL DEFAULT '[]',

    evaluated_at                 TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    evaluated_for_principal_id   VARCHAR(255) NOT NULL,
    correlation_id               VARCHAR(255) NOT NULL
);

CREATE INDEX idx_evidence_evaluations_tenant ON evidence_evaluations (tenant_id);
CREATE INDEX idx_evidence_evaluations_action
    ON evidence_evaluations (tenant_id, domain_code, action_type, evaluated_at DESC);
CREATE INDEX idx_evidence_evaluations_outcome ON evidence_evaluations (outcome);

-- Replay safety: an evaluation retried with the same correlation_id returns
-- the original determination and does not republish its event.
CREATE UNIQUE INDEX idx_evidence_evaluations_tenant_correlation
    ON evidence_evaluations (tenant_id, correlation_id);

ALTER TABLE evidence_evaluations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_evaluations
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);
