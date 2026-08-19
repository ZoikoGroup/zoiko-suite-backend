-- 0021_policy_svc.sql
-- policy-svc → schema `policy`
--
-- Squashed end state of 000001_initial_schema, 000002_add_activation_audit,
-- 000003_add_explicit_scope_type and 000004_add_control_tests. Five tables:
-- policies, policy_versions, control_test_definitions,
-- control_test_executions, attestations.
--
-- ── This service had NO row-level security at all ────────────────────────────
-- 000001 says why, and says it honestly: "policy_versions carries tenant_id but
-- is filtered by application-level scope match, not per-row RLS — reconsider
-- if/when this service takes direct multi-tenant traffic without a scope-aware
-- caller." Putting the tables behind PostgREST in a shared database is exactly
-- that moment, so RLS is added here for all five.
--
-- policy_versions uses the asymmetric nullable-tenant policy: a GLOBAL policy
-- version applies across every tenant, so any tenant being able to write one
-- would let a single tenant rewrite the approval thresholds the whole platform
-- evaluates against.
--
-- The three §E3/§E6 tables (control tests and attestations) have no tenant
-- column of their own — control_ref and subject_ref are free-text identifiers
-- backed by no registry anywhere on the platform. They are backend-only,
-- readable but not writable by console sessions.
--
-- ── The 000003 backfill is dropped ───────────────────────────────────────────
-- It derived scope_type from the existing nullness pattern of rows written
-- before the column existed. There are none here, so scope_type is simply NOT
-- NULL with no default — a writer must state the scope rather than fall into
-- 'GLOBAL', which is the point §F1 was making: "GLOBAL is an explicit scope,
-- not a default."

CREATE SCHEMA IF NOT EXISTS policy;

COMMENT ON SCHEMA policy IS
    'policy-svc. Policies and effective-dated versions, plus control test definitions/executions and attestations.';

GRANT USAGE ON SCHEMA policy TO zoiko_backend, authenticated;

-- ── policies ─────────────────────────────────────────────────────────────────

CREATE TABLE policy.policies (
    policy_id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Stable human-readable identifier and idempotent-creation dedup key.
    -- DATA ONLY, never a code switch/case.
    policy_code             VARCHAR(128) NOT NULL,

    policy_name             TEXT         NOT NULL,

    -- APPROVAL_THRESHOLD | SPEND_CONTROL | SOD_RULE | SIGNATORY_MATRIX and
    -- whatever else arrives by data migration. Only the evaluation handler
    -- switches on this, and only for the types it actually implements.
    policy_type             VARCHAR(64)  NOT NULL,

    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL DEFAULT app.current_principal_id()
);

CREATE UNIQUE INDEX idx_policies_code_unique ON policy.policies (policy_code);
CREATE INDEX idx_policies_type ON policy.policies (policy_type);

-- ── policy_versions ──────────────────────────────────────────────────────────

CREATE TABLE policy.policy_versions (
    policy_version_id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_id                 UUID        NOT NULL REFERENCES policy.policies(policy_id),

    -- NULL tenant_id = global, applying across all tenants.
    tenant_id                 UUID,
    -- NULL legal_entity_id = the whole tenant, or globally if tenant is NULL.
    legal_entity_id           UUID,

    -- "GLOBAL is an explicit scope, not a default." The CHECK ties scope_type
    -- to the nullness of the two columns above so the two can never drift.
    scope_type                VARCHAR(16) NOT NULL,

    -- Rule content; shape depends on the owning policy's policy_type, e.g.
    -- {"threshold_amount": 5000} for APPROVAL_THRESHOLD.
    rule_payload              JSONB       NOT NULL DEFAULT '{}',

    -- Point-in-time queries use the half-open interval:
    --   effective_from <= $at AND (effective_to IS NULL OR effective_to > $at)
    effective_from            TIMESTAMPTZ NOT NULL,
    effective_to              TIMESTAMPTZ,

    -- DRAFT | ACTIVE | SUPERSEDED | RETIRED
    version_status            VARCHAR(32) NOT NULL DEFAULT 'DRAFT',

    -- NULL means never activated (still DRAFT). Set once on the version's only
    -- real activation transition and never overwritten — when the version is
    -- later superseded, its own activation history stands unchanged.
    activated_by_principal_id TEXT,
    activated_at              TIMESTAMPTZ,

    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id   TEXT        NOT NULL DEFAULT app.current_principal_id(),

    CONSTRAINT chk_policy_versions_scope_type CHECK (
        (scope_type = 'GLOBAL'       AND tenant_id IS NULL     AND legal_entity_id IS NULL)
     OR (scope_type = 'TENANT'       AND tenant_id IS NOT NULL AND legal_entity_id IS NULL)
     OR (scope_type = 'LEGAL_ENTITY' AND tenant_id IS NOT NULL AND legal_entity_id IS NOT NULL)
    ),

    CONSTRAINT policy_versions_status_known
        CHECK (version_status IN ('DRAFT', 'ACTIVE', 'SUPERSEDED', 'RETIRED')),

    CONSTRAINT policy_versions_period_ordered
        CHECK (effective_to IS NULL OR effective_to > effective_from),

    -- Activation is an attributed event: a version recorded as activated with
    -- no actor, or an actor with no timestamp, is half a record.
    CONSTRAINT policy_versions_activation_is_attributed
        CHECK ((activated_at IS NULL) = (activated_by_principal_id IS NULL)),

    -- A version that has never been activated cannot be ACTIVE or SUPERSEDED.
    CONSTRAINT policy_versions_live_was_activated
        CHECK (version_status IN ('DRAFT', 'RETIRED') OR activated_at IS NOT NULL)
);

-- Idempotent creation key for a version within a policy + scope +
-- effective_from. This exact expression list is targeted BY NAME in
-- pg_store.go's ON CONFLICT clause — keep them in sync if either changes.
CREATE UNIQUE INDEX idx_policy_versions_dedup ON policy.policy_versions (
    policy_id,
    COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
    COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID),
    effective_from
);

CREATE INDEX idx_policy_versions_scope
    ON policy.policy_versions (policy_id, tenant_id, legal_entity_id);

-- At most one ACTIVE version per scope at any time. Activation supersedes the
-- prior ACTIVE version in the SAME transaction, superseding first, so this is
-- never violated mid-transaction.
CREATE UNIQUE INDEX idx_policy_versions_one_active_per_scope
    ON policy.policy_versions (
        policy_id,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
        COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID)
    )
    WHERE version_status = 'ACTIVE';

CREATE INDEX idx_policy_versions_history
    ON policy.policy_versions (policy_id, effective_from DESC);

-- ── control_test_definitions ─────────────────────────────────────────────────
-- A control is not effective because it exists. DESIGN_STATUS lives here — a
-- control has a defined test methodology or it does not. Immutable once
-- created: a changed methodology is a new row.

CREATE TABLE policy.control_test_definitions (
    control_test_definition_id UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Free-text control identifier. This service owns no control registry —
    -- that concept exists nowhere in the codebase — so it is DATA, never a
    -- foreign key and never a switch/case.
    control_ref                VARCHAR(128) NOT NULL,

    test_code                  VARCHAR(128) NOT NULL,
    title                      TEXT         NOT NULL,

    methodology                TEXT         NOT NULL,
    sample_approach            TEXT,

    -- DESIGN | AD_HOC | CONTINUOUS
    test_frequency             VARCHAR(32)  NOT NULL DEFAULT 'AD_HOC',

    -- DESIGNED | RETIRED. Deliberately NOT effective/ineffective — that is
    -- OPERATING_EFFECTIVENESS, derived from executions, never this column.
    design_status              VARCHAR(32)  NOT NULL DEFAULT 'DESIGNED',

    created_at                 TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id    TEXT         NOT NULL DEFAULT app.current_principal_id(),

    CONSTRAINT control_test_definitions_design_status_known
        CHECK (design_status IN ('DESIGNED', 'RETIRED'))
);

CREATE UNIQUE INDEX idx_control_test_definitions_code_unique
    ON policy.control_test_definitions (test_code);

CREATE INDEX idx_control_test_definitions_control_ref
    ON policy.control_test_definitions (control_ref)
    WHERE design_status = 'DESIGNED';

-- ── control_test_executions ──────────────────────────────────────────────────
-- One actual test run. operating_effectiveness is DERIVED from these rows — the
-- latest execution's result for a control_ref — and never stored redundantly on
-- the control itself.

CREATE TABLE policy.control_test_executions (
    control_test_execution_id  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    control_test_definition_id UUID        NOT NULL
        REFERENCES policy.control_test_definitions(control_test_definition_id),

    period_start               TIMESTAMPTZ NOT NULL,
    period_end                 TIMESTAMPTZ NOT NULL,

    population_description     TEXT,
    sample_description         TEXT,
    procedure_notes            TEXT,

    -- Evidence REFERENCES to the source evidence store — the evidence itself is
    -- never inlined here.
    evidence_refs              JSONB       NOT NULL DEFAULT '[]',

    tester_principal_id        TEXT        NOT NULL,

    -- EFFECTIVE | INEFFECTIVE | EXCEPTIONS_NOTED. This IS the
    -- OPERATING_EFFECTIVENESS signal, and it lives ONLY here.
    result                     VARCHAR(32) NOT NULL,
    exceptions_noted           TEXT,

    reviewer_principal_id      TEXT,
    reviewed_at                TIMESTAMPTZ,

    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id    TEXT        NOT NULL DEFAULT app.current_principal_id(),

    CONSTRAINT control_test_executions_result_known
        CHECK (result IN ('EFFECTIVE', 'INEFFECTIVE', 'EXCEPTIONS_NOTED')),

    CONSTRAINT control_test_executions_period_ordered
        CHECK (period_end > period_start),

    CONSTRAINT control_test_executions_evidence_is_array
        CHECK (jsonb_typeof(evidence_refs) = 'array'),

    -- A result of EXCEPTIONS_NOTED that notes no exceptions is not a result.
    CONSTRAINT control_test_executions_exceptions_are_described
        CHECK (result <> 'EXCEPTIONS_NOTED'
               OR (exceptions_noted IS NOT NULL AND btrim(exceptions_noted) <> '')),

    -- Review is an attributed event, both halves or neither.
    CONSTRAINT control_test_executions_review_is_attributed
        CHECK ((reviewed_at IS NULL) = (reviewer_principal_id IS NULL)),

    -- A tester may not review their own test. Segregation of duties is the
    -- entire reason an execution carries a reviewer field separate from a
    -- tester one.
    CONSTRAINT control_test_executions_reviewer_differs_from_tester
        CHECK (reviewer_principal_id IS NULL OR reviewer_principal_id <> tester_principal_id)
);

CREATE INDEX idx_control_test_executions_definition
    ON policy.control_test_executions (control_test_definition_id, period_end DESC);

CREATE TRIGGER control_test_executions_immutable
    BEFORE UPDATE OR DELETE ON policy.control_test_executions
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── attestations ─────────────────────────────────────────────────────────────
-- A signed, attributed assertion with scope and evidence — NOT automatic proof.
-- An attestation is never inferred from a control_test_execution's result and
-- never marks one effective on its own; the two objects stay independent.

CREATE TABLE policy.attestations (
    attestation_id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    statement               TEXT         NOT NULL,
    statement_version       VARCHAR(64)  NOT NULL,

    -- What this attestation is about — a control_ref, an obligation_id, a
    -- policy_code. Same "no registry backs it" doctrine as control_ref.
    subject_ref             VARCHAR(255) NOT NULL,

    period_start            TIMESTAMPTZ  NOT NULL,
    period_end              TIMESTAMPTZ  NOT NULL,

    signer_principal_id     TEXT         NOT NULL,
    -- CONTROL_OWNER | CFO | … — data only.
    signer_role             VARCHAR(64)  NOT NULL,
    signed_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    evidence_refs           JSONB        NOT NULL DEFAULT '[]',
    expires_at              TIMESTAMPTZ,

    -- ACTIVE | CHALLENGED | REVOKED. A challenge or revocation is a status
    -- transition on this row, never a delete.
    attestation_status      VARCHAR(32)  NOT NULL DEFAULT 'ACTIVE',
    revocation_reason       TEXT,

    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL DEFAULT app.current_principal_id(),

    CONSTRAINT attestations_status_known
        CHECK (attestation_status IN ('ACTIVE', 'CHALLENGED', 'REVOKED')),

    CONSTRAINT attestations_period_ordered
        CHECK (period_end > period_start),

    CONSTRAINT attestations_evidence_is_array
        CHECK (jsonb_typeof(evidence_refs) = 'array'),

    -- A revoked attestation must say why. Withdrawing an assertion without a
    -- reason leaves the register unable to explain what changed.
    CONSTRAINT attestations_revoked_has_reason
        CHECK (attestation_status <> 'REVOKED'
               OR (revocation_reason IS NOT NULL AND btrim(revocation_reason) <> ''))
);

CREATE INDEX idx_attestations_subject
    ON policy.attestations (subject_ref, period_end DESC);

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE policy.policies                   ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy.policies                   FORCE  ROW LEVEL SECURITY;
ALTER TABLE policy.policy_versions            ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy.policy_versions            FORCE  ROW LEVEL SECURITY;
ALTER TABLE policy.control_test_definitions   ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy.control_test_definitions   FORCE  ROW LEVEL SECURITY;
ALTER TABLE policy.control_test_executions    ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy.control_test_executions    FORCE  ROW LEVEL SECURITY;
ALTER TABLE policy.attestations               ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy.attestations               FORCE  ROW LEVEL SECURITY;

-- policies is the named container and carries no scope of its own.
CREATE POLICY backend_all ON policy.policies
    FOR ALL TO zoiko_backend USING (true) WITH CHECK (true);
CREATE POLICY catalogue_read ON policy.policies
    FOR SELECT TO authenticated USING (true);

-- Asymmetric, as in configuration-feature-flag: a GLOBAL version applies to
-- every tenant, so a tenant connection must never be able to write one.
CREATE POLICY tenant_isolation ON policy.policy_versions
    FOR ALL
    TO zoiko_backend
    USING (tenant_id IS NULL OR tenant_id::text = app.current_tenant_id())
    WITH CHECK (
        (tenant_id IS NOT NULL AND tenant_id::text = app.current_tenant_id())
        OR (tenant_id IS NULL AND app.current_tenant_id() IS NULL)
    );

CREATE POLICY tenant_read ON policy.policy_versions
    FOR SELECT
    TO authenticated
    USING (tenant_id IS NULL OR tenant_id::text = app.current_tenant_id());

CREATE POLICY backend_all ON policy.control_test_definitions
    FOR ALL TO zoiko_backend USING (true) WITH CHECK (true);
CREATE POLICY catalogue_read ON policy.control_test_definitions
    FOR SELECT TO authenticated USING (true);

CREATE POLICY backend_all ON policy.control_test_executions
    FOR ALL TO zoiko_backend USING (true) WITH CHECK (true);
CREATE POLICY catalogue_read ON policy.control_test_executions
    FOR SELECT TO authenticated USING (true);

CREATE POLICY backend_all ON policy.attestations
    FOR ALL TO zoiko_backend USING (true) WITH CHECK (true);
CREATE POLICY catalogue_read ON policy.attestations
    FOR SELECT TO authenticated USING (true);

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON policy.policies                 TO authenticated;
GRANT SELECT ON policy.policy_versions          TO authenticated;
GRANT SELECT ON policy.control_test_definitions TO authenticated;
GRANT SELECT ON policy.control_test_executions  TO authenticated;
GRANT SELECT ON policy.attestations             TO authenticated;

-- policies and control_test_definitions are immutable once created: a change is
-- a new row, so neither gets UPDATE.
GRANT SELECT, INSERT         ON policy.policies                 TO zoiko_backend;
GRANT SELECT, INSERT         ON policy.control_test_definitions TO zoiko_backend;

-- policy_versions transitions DRAFT → ACTIVE → SUPERSEDED → RETIRED.
GRANT SELECT, INSERT, UPDATE ON policy.policy_versions          TO zoiko_backend;

-- An execution is the record of a test that was run; it is never revised.
GRANT SELECT, INSERT         ON policy.control_test_executions  TO zoiko_backend;

-- An attestation transitions ACTIVE → CHALLENGED / REVOKED.
GRANT SELECT, INSERT, UPDATE ON policy.attestations             TO zoiko_backend;
