-- 000001_initial_schema.up.sql
-- privacy-transfer-svc — initial schema (PRV-05, ZS-SVC-W-001 §16/§17).
--
-- Fifth and final ZS-SVC-W-001 service. Five tables:
--   processor_relationships / subprocessors — a real inventory registry
--     (§16.1's own field list), simpler than PRV-01's DRAFT/APPROVED/
--     ACTIVE lifecycle since the spec names no such workflow for these —
--     just ACTIVE/INACTIVE.
--   transfer_mechanisms — a directly-registered legal-mechanism record
--     (SCC/BCR/adequacy/derogation), NOT a resolved PDC catalogue entry —
--     see internal/domain's package doc comment for why.
--   transfer_assessments — append-only DPIA/TIA review evidence, same
--     doctrine as every evidence table in this domain: a re-assessment
--     is a new row, never an edit of a prior one.
--   transfer_decisions — the append-only decision log, same "decision
--     durability" doctrine as PRV-03's privacy_decisions.
CREATE TABLE processor_relationships (
    relationship_id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID,

    controller_ref            TEXT        NOT NULL,
    processor_ref             TEXT        NOT NULL,
    service                   TEXT        NOT NULL,
    processing_instructions   TEXT,
    -- References privacy-purpose-registry-svc's activity_id values — NOT
    -- a local foreign key (validated at write time via a live HTTP call,
    -- see internal/purposeregistry).
    purpose_activity_refs     JSONB       NOT NULL DEFAULT '[]',
    data_categories           JSONB       NOT NULL DEFAULT '[]',
    subject_classes           JSONB       NOT NULL DEFAULT '[]',
    contract_evidence_ref     TEXT,
    jurisdictions             JSONB       NOT NULL DEFAULT '[]',

    -- ACTIVE | INACTIVE.
    status                    VARCHAR(16) NOT NULL DEFAULT 'ACTIVE',

    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id   TEXT        NOT NULL
);

CREATE TABLE subprocessors (
    subprocessor_id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                   UUID,
    relationship_id             UUID        NOT NULL REFERENCES processor_relationships(relationship_id),

    provider_identity           TEXT        NOT NULL,
    service                     TEXT        NOT NULL,
    purpose                     TEXT,
    data_scope                  TEXT,
    processing_locations        JSONB       NOT NULL DEFAULT '[]',
    onward_subprocessors        JSONB       NOT NULL DEFAULT '[]',
    notification_approval_model TEXT,
    contract_evidence_ref       TEXT,

    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id     TEXT        NOT NULL
);

CREATE INDEX idx_subprocessors_relationship ON subprocessors (relationship_id);

CREATE TABLE transfer_mechanisms (
    mechanism_id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID,

    -- Data only — e.g. SCC, BCR, ADEQUACY, DEROGATION, OTHER.
    mechanism_type            VARCHAR(32) NOT NULL,
    evidence_ref              TEXT,
    conditions                TEXT,
    valid_from                TIMESTAMPTZ NOT NULL,
    valid_until               TIMESTAMPTZ, -- NULL = no expiry

    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id   TEXT        NOT NULL
);

CREATE TABLE transfer_assessments (
    assessment_id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID,
    relationship_id           UUID        NOT NULL REFERENCES processor_relationships(relationship_id),

    -- APPROVE | REMEDIATE | REJECT.
    outcome                   VARCHAR(16) NOT NULL,
    reviewer_principal_id     TEXT        NOT NULL,
    residual_risk             TEXT,
    evidence_ref              TEXT,
    -- §17.1: "Expiry/review date reached... marks prior authorization
    -- stale" — one of the spec's own mandatory reassessment triggers.
    review_trigger_at         TIMESTAMPTZ,

    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_transfer_assessments_relationship ON transfer_assessments (relationship_id, created_at DESC);

CREATE TABLE transfer_decisions (
    decision_id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID,

    relationship_id           UUID        NOT NULL REFERENCES processor_relationships(relationship_id),
    transfer_mechanism_id     UUID        NOT NULL REFERENCES transfer_mechanisms(mechanism_id),
    destination_jurisdiction  VARCHAR(16),
    assessment_id             UUID        REFERENCES transfer_assessments(assessment_id),

    -- AUTHORIZED | CONDITIONAL | BLOCKED | REVIEW_REQUIRED. CONDITIONAL
    -- is reserved in the column's domain but never written by this
    -- version — see internal/domain's package doc comment.
    result                    VARCHAR(32) NOT NULL,
    reason_codes              JSONB       NOT NULL DEFAULT '[]',

    actor_principal_id        TEXT        NOT NULL,
    correlation_id            TEXT,

    decided_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_transfer_decisions_relationship ON transfer_decisions (relationship_id, decided_at DESC);
