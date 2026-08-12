-- 000004_add_control_tests.up.sql
-- Policy Service — control test definitions and executions
--
-- docs/original_doc/zoiko_suite_doc7.txt §E3: "Can a control be marked
-- effective because it exists? Decision: No... Separate DESIGN_STATUS and
-- OPERATING_EFFECTIVENESS. Effectiveness requires test/evidence under an
-- approved methodology and review cadence." §I3: "control_test_execution
-- stores test_definition_version, period, population/sample, procedure,
-- evidence refs, tester, result, exceptions, reviewer and timestamps."
--
-- Owns:
--   control_test_definitions — repeatable test methodology per control.
--     design_status lives HERE: a control has a defined test methodology
--     or it doesn't. Immutable once created (same doctrine as `policies`
--     in 000001) — no UPDATE/DELETE; a changed methodology is a new row.
--   control_test_executions  — one actual test run against a definition.
--     operating_effectiveness is DERIVED from these rows (the latest
--     execution's result for a control_ref), never stored redundantly on
--     the control itself — see GET /v1/controls/{control_ref}/effectiveness.
--
-- control_ref is a free-text identifier for the control being tested — this
-- service owns no control registry of its own (that concept does not exist
-- anywhere in the codebase yet), so it is DATA, never a foreign key or a
-- code switch/case, same doctrine as policy_type/obligation_type elsewhere.
--
-- test_definition_version: this service does not implement per-definition
-- version bumps (a known v1 simplification — see PROGRESS.md) — a changed
-- methodology is a new control_test_definitions row, so
-- control_test_executions.control_test_definition_id already IS the
-- version reference, the same way PolicyVersion's FK to PolicyID doubles
-- as its own versioning axis.

-- ── control_test_definitions ─────────────────────────────────────────────────

CREATE TABLE control_test_definitions (
    control_test_definition_id UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Free-text control identifier — DATA ONLY, no registry backs it yet.
    control_ref                 VARCHAR(128) NOT NULL,

    -- Idempotent creation dedup key.
    test_code                    VARCHAR(128) NOT NULL,

    title                         TEXT        NOT NULL,

    -- Test procedure/approach — free text, doc7 §I3's "procedure".
    methodology                    TEXT        NOT NULL,
    sample_approach                  TEXT,

    -- DESIGN | AD_HOC | CONTINUOUS — DATA ONLY, not an enum.
    test_frequency                    VARCHAR(32) NOT NULL DEFAULT 'AD_HOC',

    -- §E3's DESIGN_STATUS: does a defined test methodology exist for this
    -- control at all. DESIGNED | RETIRED — DATA ONLY. This is deliberately
    -- NOT "effective"/"ineffective" — that is OPERATING_EFFECTIVENESS,
    -- derived from control_test_executions, never this column.
    design_status                       VARCHAR(32) NOT NULL DEFAULT 'DESIGNED',

    created_at                             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                   TEXT        NOT NULL
);

CREATE UNIQUE INDEX idx_control_test_definitions_code_unique
    ON control_test_definitions (test_code);

CREATE INDEX idx_control_test_definitions_control_ref
    ON control_test_definitions (control_ref)
    WHERE design_status = 'DESIGNED';

-- ── control_test_executions ──────────────────────────────────────────────────

CREATE TABLE control_test_executions (
    control_test_execution_id  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    control_test_definition_id  UUID        NOT NULL REFERENCES control_test_definitions(control_test_definition_id),

    period_start                  TIMESTAMPTZ NOT NULL,
    period_end                     TIMESTAMPTZ NOT NULL,

    population_description           TEXT,
    sample_description                  TEXT,
    procedure_notes                        TEXT,

    -- List of evidence reference strings (doc7 §I2/§I3 — evidence refs link
    -- to the source evidence store, never inline the evidence itself).
    evidence_refs                             JSONB       NOT NULL DEFAULT '[]',

    tester_principal_id                          TEXT        NOT NULL,

    -- EFFECTIVE | INEFFECTIVE | EXCEPTIONS_NOTED — DATA ONLY. This IS the
    -- OPERATING_EFFECTIVENESS signal §E3 requires, and it lives ONLY here —
    -- never duplicated onto control_test_definitions or any control row.
    result                                           VARCHAR(32) NOT NULL,
    exceptions_noted                                    TEXT,

    reviewer_principal_id                                  TEXT,
    reviewed_at                                               TIMESTAMPTZ,

    created_at                                                   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                                         TEXT        NOT NULL
);

CREATE INDEX idx_control_test_executions_definition
    ON control_test_executions (control_test_definition_id, period_end DESC);

-- ── attestations ──────────────────────────────────────────────────────────────
-- doc7 §E6: "As signed/attributed assertions with scope and evidence, not
-- automatic proof... statement/version, subject, period, signer, role,
-- timestamp, evidence refs, expiry and challenge/revocation state."
-- An attestation is a REPRESENTATION by an identified actor — it is never
-- inferred from a control_test_execution's result and never marks one
-- effective on its own; the two objects stay independent, exactly as
-- control_test_definitions/executions stay independent of any future
-- control-registry "exists" flag per §E3.

CREATE TABLE attestations (
    attestation_id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    statement                 TEXT        NOT NULL,
    -- Version of the statement text/template being attested to — free text,
    -- DATA ONLY (e.g. "v3", "2026-Q2").
    statement_version          VARCHAR(64) NOT NULL,

    -- What this attestation is about — free-text subject reference (a
    -- control_ref, an obligation_id, a policy_code, etc.), same "no
    -- registry backs it" doctrine as control_ref above.
    subject_ref                 VARCHAR(255) NOT NULL,

    period_start                  TIMESTAMPTZ NOT NULL,
    period_end                     TIMESTAMPTZ NOT NULL,

    signer_principal_id              TEXT        NOT NULL,
    -- Role the signer attested in — DATA ONLY (e.g. "CONTROL_OWNER", "CFO").
    signer_role                        VARCHAR(64) NOT NULL,
    signed_at                            TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    evidence_refs                          JSONB       NOT NULL DEFAULT '[]',
    expires_at                                TIMESTAMPTZ,

    -- ACTIVE | CHALLENGED | REVOKED — DATA ONLY. §E6's "challenge/revocation
    -- state" — an attestation is append-only otherwise; a challenge or
    -- revocation is a status transition on this row, never a delete.
    attestation_status                           VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    revocation_reason                               TEXT,

    created_at                                         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                               TEXT        NOT NULL
);

CREATE INDEX idx_attestations_subject
    ON attestations (subject_ref, period_end DESC);
