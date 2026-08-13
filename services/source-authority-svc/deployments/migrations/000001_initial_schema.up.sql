-- 000001_initial_schema.up.sql
-- Source Authority Service — initial schema
--
-- docs/original_doc/zoiko_suite_doc7.txt §D2: "source_authority_map
-- defines source, field family, precedence, effective dates, conflict
-- route and allowed correction path. Ambiguous material facts block
-- downstream high-impact decisions." §K1: "normalized_fact retains
-- source_system, source_record, source_version, observed_at, effective_at,
-- transformation version and authority class." §D1: "Staleness is an
-- exception to reconcile, not permission to seize authority... never
-- silently back-write without an approved integration action." §D3:
-- "cached/derived data [can become authoritative] only for explicitly
-- defined derived facts."
--
-- Owns:
--   source_authority_maps — versioned precedence rules: for a given field
--     family, which connected system's value wins when sources disagree.
--     Immutable once created, same doctrine as retention_policies.
--   normalized_facts — append-only ingestion record of one fact as
--     reported by one source system at one point in time. This service
--     never "corrects" a fact in place — a correction is a NEW fact row
--     with a later observed_at, never an UPDATE (§D1's "never silently
--     back-write").
--
-- Resolution (see internal/store's ResolveAuthoritativeFact) composes
-- both: for a (field_family, entity_ref) pair, find every source's latest
-- applicable fact, rank by source_authority_maps.precedence_rank, and
-- return the top-ranked one — UNLESS two sources tied for top rank
-- disagree, in which case doc7 §D2 requires blocking rather than
-- guessing.

-- ── source_authority_maps ────────────────────────────────────────────────────

CREATE TABLE source_authority_maps (
    source_authority_map_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Free-text field-family identifier — DATA ONLY, no registry backs it
    -- yet (same doctrine as kill-switch-registry-svc's domain column).
    -- E.g. PAYROLL_GROSS_PAY, HR_EMPLOYMENT_STATUS, BILLING_CONTACT_EMAIL.
    field_family                VARCHAR(128) NOT NULL,

    -- The connected system this precedence rule is FOR — e.g. "ZoikoLogia",
    -- "Kriton", "ADP". DATA ONLY.
    source_system                 VARCHAR(128) NOT NULL,

    -- Lower number = higher precedence (rank 1 beats rank 2). §D2's
    -- "precedence".
    precedence_rank                  INTEGER     NOT NULL,

    conflict_route                      TEXT        NOT NULL,
    allowed_correction_path                TEXT,

    effective_from                          TIMESTAMPTZ NOT NULL,
    effective_to                               TIMESTAMPTZ,

    created_at                                    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                          TEXT        NOT NULL
);

CREATE UNIQUE INDEX idx_source_authority_maps_unique
    ON source_authority_maps (field_family, source_system, effective_from);

CREATE INDEX idx_source_authority_maps_lookup
    ON source_authority_maps (field_family, source_system);

-- ── normalized_facts ──────────────────────────────────────────────────────────

CREATE TABLE normalized_facts (
    normalized_fact_id      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    field_family               VARCHAR(128) NOT NULL,
    -- Which business entity this fact is about — free-text reference, no
    -- registry backs it (same doctrine as kill-switch-registry-svc's
    -- tenant_id being an opaque scope value rather than an FK it owns).
    entity_ref                   VARCHAR(255) NOT NULL,

    source_system                   VARCHAR(128) NOT NULL,
    -- The source system's OWN identifier for the record this fact came
    -- from — §K1's "source_record".
    source_record                      VARCHAR(255) NOT NULL,
    source_version                        VARCHAR(64),

    fact_value                               JSONB       NOT NULL,

    observed_at                                 TIMESTAMPTZ NOT NULL,
    effective_at                                   TIMESTAMPTZ NOT NULL,

    transformation_version                            VARCHAR(64),

    -- AUTHORITATIVE | DERIVED | CACHED — DATA ONLY. §D3: cached/derived
    -- data may become authoritative ONLY for explicitly defined derived
    -- facts — this column records which of the three a given fact is,
    -- it does not itself decide whether DERIVED/CACHED facts count in
    -- resolution (that decision is the caller's, per doc7's own framing
    -- of "explicitly defined" as a case-by-case approval, not a blanket
    -- rule this service can invent on its behalf).
    authority_class                                   VARCHAR(32) NOT NULL DEFAULT 'AUTHORITATIVE',

    created_at                                            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                                  TEXT        NOT NULL
);

CREATE INDEX idx_normalized_facts_lookup
    ON normalized_facts (field_family, entity_ref, source_system, effective_at DESC);
