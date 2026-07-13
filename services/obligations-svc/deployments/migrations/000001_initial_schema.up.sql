-- 000001_initial_schema.up.sql
-- Obligations Service — initial schema
--
-- Owns:
--   obligations          — statutory/regulatory/contractual/internal policy
--                           obligations linked to operational actions
--   filing_requirements  — filing obligations scoped under a parent obligation
--
-- Design decisions (mirrors policy-svc's Policy/PolicyVersion shape):
--   - obligation_source_type, obligation_type, obligation_status, and
--     severity_level are all VARCHAR — no enums. New values are added via
--     data only, never a code change.
--   - No soft-delete, no hard DELETE on either table. An obligation is
--     closed via an obligation_status transition to CLOSED (closed_at
--     stamped), never removed.
--   - Critical constraint (03-microservices.md §8.5): legal_entity_id and
--     jurisdiction_id are NOT NULL on every obligation — entity-bound and
--     jurisdiction-bound, no exceptions.
--   - Critical enhancement "Atomic Linking": source_reference is NOT NULL —
--     every obligation must point to its originating source.

-- ── obligations ──────────────────────────────────────────────────────────────

CREATE TABLE obligations (
    obligation_id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Critical constraint: entity-bound and jurisdiction-bound, always.
    legal_entity_id          UUID        NOT NULL,
    jurisdiction_id          UUID        NOT NULL,

    -- Identifies the originating record (e.g. "CONTRACT_CLAUSE",
    -- "FILING_RULE", "POLICY_MANDATE", "JURISDICTION_RULE") and its ID in
    -- whatever service owns that source. Not a foreign key — the source may
    -- live in a different service entirely.
    obligation_source_type   VARCHAR(64) NOT NULL,
    obligation_source_id     TEXT        NOT NULL,

    -- Stable, human-readable identifier — idempotent creation dedup key.
    -- DATA ONLY, never used as a code switch/case.
    obligation_code          VARCHAR(128) NOT NULL,

    -- VARCHAR — extensible via data migration only (e.g. "FILING",
    -- "TAX_PAYMENT", "REGULATORY_REPORT").
    obligation_type          VARCHAR(64) NOT NULL,

    -- OPEN | IN_PROGRESS | OVERDUE | CLOSED — a real state machine, enforced
    -- in application code (store.transitionObligationStatus), not a DB CHECK
    -- constraint, mirroring policy_versions.version_status's approach.
    obligation_status        VARCHAR(32) NOT NULL DEFAULT 'OPEN',

    due_date                 TIMESTAMPTZ NOT NULL,

    -- Data only (e.g. "LOW", "MEDIUM", "HIGH", "CRITICAL").
    severity_level           VARCHAR(32) NOT NULL,

    -- Free-text/data tag identifying the owning business function.
    responsible_function     TEXT        NOT NULL,

    -- Atomic Linking — every obligation must point to its originating
    -- source. Never empty.
    source_reference         TEXT        NOT NULL,

    -- Audit
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL,
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Nil until transitioned to CLOSED, set exactly once, never unset.
    closed_at                TIMESTAMPTZ
);

-- Idempotent creation key: same code = same obligation.
CREATE UNIQUE INDEX idx_obligations_code_unique ON obligations (obligation_code);

-- Query filters per the "list/query obligations" API.
CREATE INDEX idx_obligations_legal_entity ON obligations (legal_entity_id);
CREATE INDEX idx_obligations_jurisdiction ON obligations (jurisdiction_id);
CREATE INDEX idx_obligations_status ON obligations (obligation_status);
CREATE INDEX idx_obligations_due_date ON obligations (due_date);

-- ── filing_requirements ──────────────────────────────────────────────────────

CREATE TABLE filing_requirements (
    filing_requirement_id    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    obligation_id            UUID        NOT NULL REFERENCES obligations(obligation_id),

    filing_type              VARCHAR(64)  NOT NULL,
    filing_authority         VARCHAR(128) NOT NULL,
    submission_channel       VARCHAR(64)  NOT NULL,

    -- Data only (e.g. "PENDING", "SUBMITTED", "ACCEPTED", "REJECTED").
    filing_status            VARCHAR(32) NOT NULL DEFAULT 'PENDING',

    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Primary lookup: filing requirements for a given obligation.
CREATE INDEX idx_filing_requirements_obligation ON filing_requirements (obligation_id);
