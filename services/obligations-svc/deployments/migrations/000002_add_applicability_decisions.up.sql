-- 000002_add_applicability_decisions.up.sql
-- Obligations Service — applicability_decision
--
-- docs/original_doc/zoiko_suite_doc7.txt §E2: "What makes an obligation
-- applicable? Decision: A versioned applicability decision based on
-- jurisdiction, entity, activity, product/process, thresholds, dates and
-- approved source rules... Store applicability_decision with source/rule
-- versions, effective dates, facts used, actor/system, confidence/
-- uncertainty and review requirement. Unknown applicability does not
-- default to NOT_APPLICABLE."
--
-- Owns:
--   applicability_decisions — append-only decision log for one obligation's
--     applicability in a given jurisdiction/entity/activity/product scope.
--     "Versioned" here means new facts/rules always produce a NEW row, never
--     an UPDATE of a prior decision — the same immutable-history doctrine as
--     policy_versions/control_test_executions elsewhere in this codebase.
--     "Current" applicability for a scope is derived by querying the latest
--     row whose effective window covers now — see FindCurrentApplicability.
--
-- decision is VARCHAR — UNASSESSED | APPLICABLE | NOT_APPLICABLE | UNCERTAIN
-- — DATA ONLY, no code switch/case. Critically, UNASSESSED (no decision has
-- ever been made) and NOT_APPLICABLE (a decision was made and it concluded
-- the obligation does not apply) are two DIFFERENT, DELIBERATELY DISTINCT
-- values: an absent row (nothing to query) must read as UNASSESSED at the
-- application layer, never silently coerced into NOT_APPLICABLE.

CREATE TABLE applicability_decisions (
    applicability_decision_id UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    obligation_id               UUID        NOT NULL REFERENCES obligations(obligation_id),

    jurisdiction_code             VARCHAR(64)  NOT NULL,
    entity_ref                      VARCHAR(255) NOT NULL,
    -- Optional finer-grained facts this decision was scoped against.
    activity_ref                       VARCHAR(255),
    product_process_ref                  VARCHAR(255),

    -- APPLICABLE | NOT_APPLICABLE | UNCERTAIN — a row always represents a
    -- decision that WAS made. UNASSESSED is never stored; it is the
    -- application-level answer when no row exists for a scope at all (see
    -- doc comment above).
    decision                                VARCHAR(32) NOT NULL,

    -- Which approved source rule justified this decision, and its version —
    -- doc7 §E2's "approved source rules" + "source/rule versions".
    source_rule_ref                            VARCHAR(255) NOT NULL,
    source_rule_version                           VARCHAR(64)  NOT NULL,

    effective_from                                   TIMESTAMPTZ NOT NULL,
    effective_to                                        TIMESTAMPTZ,

    -- The facts actually used to reach this decision — doc7 §E2's "facts
    -- used". JSONB so the shape can vary by jurisdiction/obligation type
    -- without a schema change.
    facts_used                                             JSONB       NOT NULL DEFAULT '{}',

    confidence                                                NUMERIC(5,4),
    uncertainty_notes                                            TEXT,

    -- "actor/system" — exactly one of these is set: a human decided it, or
    -- an automated rule engine did.
    decided_by_principal_id                                        TEXT,
    decided_by_system                                                 VARCHAR(128),

    -- doc7 §E2's "review requirement" — a decision can require human review
    -- even after being reached (e.g. UNCERTAIN outcomes, or low confidence).
    review_required                                                     BOOLEAN NOT NULL DEFAULT FALSE,

    created_at                                                             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                                                   TEXT        NOT NULL,

    CONSTRAINT chk_applicability_decisions_actor_or_system CHECK (
        decided_by_principal_id IS NOT NULL OR decided_by_system IS NOT NULL
    )
);

-- Primary lookup: current applicability for an obligation+scope, most
-- recent effective_from first.
CREATE INDEX idx_applicability_decisions_scope
    ON applicability_decisions (obligation_id, jurisdiction_code, entity_ref, effective_from DESC);
