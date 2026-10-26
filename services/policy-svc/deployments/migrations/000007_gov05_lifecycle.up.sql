-- 000007_gov05_lifecycle.up.sql
-- Policy Service — GOV-05 lifecycle, bitemporal resolution, local evidence
--
-- Closes the Governance Platform audit (2026-09-28, section 3/7) rows that
-- need schema:
--   * Policy families are tenant-owned (Doc 04 §7.1, V-001 §25). tenant_id
--     already exists (000006) but was never written; policy_code becomes
--     unique per owner instead of platform-wide, and policies get RLS.
--   * GOV-05 state model Draft/Review/Approved/Scheduled/Active/Suspended/
--     Superseded/Retired, with approval recorded on the version and every
--     transition appended to policy_version_transitions.
--   * Released content is immutable in the database, not only in the API
--     (GCP §2 invariant #4, V-001 PDC-I-02).
--   * artifact_digest is server-derived from the stored payload so an
--     evaluation can verify it (V-001 §14.1 step 2).
--   * Evaluation evidence is stored locally in the same transaction as the
--     result and delivered to governance-decision-log-svc from an outbox
--     (decision C6, 14 Oct 2026).
--   * Control-test executions and attestations are tenant data.
--
-- Backfills run with FORCE ROW LEVEL SECURITY lifted, because FORCE binds a
-- non-superuser migration owner too and would hide every tenant row.

-- ── policies: tenant-owned families ──────────────────────────────────────────

DROP INDEX IF EXISTS idx_policies_code_unique;
CREATE UNIQUE INDEX idx_policies_code_per_owner
    ON policies (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID), policy_code);

ALTER TABLE policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON policies
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- ── policy_versions: lifecycle columns ───────────────────────────────────────

ALTER TABLE policy_versions
    -- Optimistic concurrency (GCP §16 expected_version). Every transition
    -- increments it; content never changes, so it counts transitions only.
    ADD COLUMN row_version              INTEGER NOT NULL DEFAULT 1,
    -- Idempotency-Key of the create request, for replay of an identical POST.
    ADD COLUMN create_request_key       TEXT,
    ADD COLUMN submitted_by_principal_id TEXT,
    ADD COLUMN submitted_at             TIMESTAMPTZ,
    ADD COLUMN approved_by_principal_id TEXT,
    ADD COLUMN approved_at              TIMESTAMPTZ,
    ADD COLUMN approval_reference       TEXT,
    -- When a suspension or retirement was recorded. Resolution ignores a
    -- version withdrawn at or before the knowledge cut.
    ADD COLUMN withdrawn_at             TIMESTAMPTZ,
    ADD COLUMN superseded_at            TIMESTAMPTZ,
    ADD COLUMN superseded_by_version_id UUID REFERENCES policy_versions (policy_version_id);

COMMENT ON COLUMN policy_versions.activated_at IS
    'When the activation (or scheduled activation) was recorded: the knowledge time from which resolution may select this version. A scheduled version governs from effective_from.';

ALTER TABLE policy_versions NO FORCE ROW LEVEL SECURITY;

-- version_number was backfilled per (policy, scope, effective_from) by 000006
-- and then always written as 1, so it never numbered anything. Renumber per
-- policy and owning tenant scope in creation order (a tenant cannot see
-- another tenant's versions of a platform family, so it could never number
-- across them); the unique index below keeps it sequential.
WITH numbered AS (
    SELECT policy_version_id,
           ROW_NUMBER() OVER (
               PARTITION BY policy_id, COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID)
               ORDER BY created_at, policy_version_id
           ) AS vn
    FROM policy_versions
)
UPDATE policy_versions pv
SET version_number = n.vn
FROM numbered n
WHERE pv.policy_version_id = n.policy_version_id;

-- The digest is recomputed from the stored payload's canonical jsonb text, the
-- same bytes an evaluation reads back, so it can be verified at evaluation.
UPDATE policy_versions
SET artifact_digest = 'sha256:' || encode(sha256(convert_to(rule_payload::text, 'UTF8')), 'hex');

ALTER TABLE policy_versions FORCE ROW LEVEL SECURITY;

ALTER TABLE policy_versions ALTER COLUMN version_number SET NOT NULL;
ALTER TABLE policy_versions ALTER COLUMN artifact_digest SET NOT NULL;

-- The (policy, scope, effective_from) dedup key made a corrected version with
-- the same effective date impossible to create. Replay is keyed on the
-- request's Idempotency-Key (or a caller-supplied policy_version_id) instead.
DROP INDEX IF EXISTS idx_policy_versions_dedup;
CREATE UNIQUE INDEX idx_policy_versions_number
    ON policy_versions (
        policy_id,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
        version_number
    );
CREATE UNIQUE INDEX idx_policy_versions_request_key
    ON policy_versions (policy_id, create_request_key)
    WHERE create_request_key IS NOT NULL;

-- At most one pending scheduled activation per scope.
CREATE UNIQUE INDEX idx_policy_versions_one_scheduled_per_scope
    ON policy_versions (
        policy_id,
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID),
        COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID)
    )
    WHERE version_status = 'SCHEDULED';

ALTER TABLE policy_versions
    ADD CONSTRAINT chk_policy_versions_status CHECK (version_status IN (
        'DRAFT', 'REVIEW', 'APPROVED', 'SCHEDULED', 'ACTIVE', 'SUSPENDED', 'SUPERSEDED', 'RETIRED'
    ));

-- NOT VALID: enforced for every new or changed row, without failing on a
-- legacy row that predates the API check.
ALTER TABLE policy_versions
    ADD CONSTRAINT chk_policy_versions_window
    CHECK (effective_to IS NULL OR effective_to > effective_from) NOT VALID;

-- Resolution candidates: every version of a type that was ever bound.
CREATE INDEX idx_policy_versions_bound
    ON policy_versions (policy_id, activated_at)
    WHERE activated_at IS NOT NULL;

-- ── server-derived digest ────────────────────────────────────────────────────

CREATE FUNCTION policy_versions_set_digest() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.artifact_digest := 'sha256:' || encode(sha256(convert_to(NEW.rule_payload::text, 'UTF8')), 'hex');
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_policy_versions_set_digest
    BEFORE INSERT ON policy_versions
    FOR EACH ROW EXECUTE FUNCTION policy_versions_set_digest();

-- ── immutability (GCP §2 #4, V-001 PDC-I-02) ─────────────────────────────────

CREATE FUNCTION policy_versions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'policy_versions is append-only: a version is never deleted'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.policy_version_id       IS DISTINCT FROM OLD.policy_version_id
       OR NEW.policy_id            IS DISTINCT FROM OLD.policy_id
       OR NEW.tenant_id            IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id      IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.scope_type           IS DISTINCT FROM OLD.scope_type
       OR NEW.rule_payload::text   IS DISTINCT FROM OLD.rule_payload::text
       OR NEW.effective_from       IS DISTINCT FROM OLD.effective_from
       OR NEW.effective_to         IS DISTINCT FROM OLD.effective_to
       OR NEW.version_number       IS DISTINCT FROM OLD.version_number
       OR NEW.source               IS DISTINCT FROM OLD.source
       OR NEW.rationale            IS DISTINCT FROM OLD.rationale
       OR NEW.artifact_digest      IS DISTINCT FROM OLD.artifact_digest
       OR NEW.known_from           IS DISTINCT FROM OLD.known_from
       OR NEW.create_request_key   IS DISTINCT FROM OLD.create_request_key
       OR NEW.created_at           IS DISTINCT FROM OLD.created_at
       OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'policy version content is immutable: create a new version instead'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_policy_versions_guard
    BEFORE UPDATE OR DELETE ON policy_versions
    FOR EACH ROW EXECUTE FUNCTION policy_versions_guard();

CREATE FUNCTION policy_svc_refuse_truncate() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: TRUNCATE is refused', TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER trg_policy_versions_no_truncate
    BEFORE TRUNCATE ON policy_versions
    FOR EACH STATEMENT EXECUTE FUNCTION policy_svc_refuse_truncate();

CREATE FUNCTION policies_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'policies is append-only: a policy family is never deleted'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.policy_id               IS DISTINCT FROM OLD.policy_id
       OR NEW.policy_code          IS DISTINCT FROM OLD.policy_code
       OR NEW.policy_name          IS DISTINCT FROM OLD.policy_name
       OR NEW.policy_type          IS DISTINCT FROM OLD.policy_type
       OR NEW.tenant_id            IS DISTINCT FROM OLD.tenant_id
       OR NEW.created_at           IS DISTINCT FROM OLD.created_at
       OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'policy identity is immutable'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_policies_guard
    BEFORE UPDATE OR DELETE ON policies
    FOR EACH ROW EXECUTE FUNCTION policies_guard();

CREATE TRIGGER trg_policies_no_truncate
    BEFORE TRUNCATE ON policies
    FOR EACH STATEMENT EXECUTE FUNCTION policy_svc_refuse_truncate();

-- ── transition log (GOV-05 evidence: approvals, activation actor/time) ───────

CREATE TABLE policy_version_transitions (
    transition_id       BIGSERIAL    PRIMARY KEY,
    policy_version_id   UUID         NOT NULL REFERENCES policy_versions (policy_version_id),
    tenant_id           UUID,
    from_status         VARCHAR(32),
    to_status           VARCHAR(32)  NOT NULL,
    actor_id            TEXT         NOT NULL,
    reason              TEXT,
    approval_reference  TEXT,
    correlation_id      TEXT,
    occurred_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_policy_version_transitions_version
    ON policy_version_transitions (policy_version_id, transition_id);

ALTER TABLE policy_version_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy_version_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON policy_version_transitions
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE FUNCTION policy_svc_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER trg_policy_version_transitions_append_only
    BEFORE UPDATE OR DELETE ON policy_version_transitions
    FOR EACH ROW EXECUTE FUNCTION policy_svc_append_only();
CREATE TRIGGER trg_policy_version_transitions_no_truncate
    BEFORE TRUNCATE ON policy_version_transitions
    FOR EACH STATEMENT EXECUTE FUNCTION policy_svc_refuse_truncate();

-- ── scheduled activations (read by the scheduler across tenants) ─────────────
-- Holds ids and a due time only, so it carries no RLS; the scheduler then
-- promotes each version inside that version's own tenant scope.

CREATE TABLE policy_activation_schedule (
    policy_version_id   UUID         PRIMARY KEY REFERENCES policy_versions (policy_version_id),
    tenant_id           UUID,
    due_at              TIMESTAMPTZ  NOT NULL,
    scheduled_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_policy_activation_schedule_due ON policy_activation_schedule (due_at);

-- ── resolution cache watermark ───────────────────────────────────────────────
-- Bumped by every statement that changes a version's lifecycle, so a cached
-- candidate set is reused only while nothing that could change a resolution
-- has happened anywhere (GCP domain negative path #30: stale policy cache
-- after suspension).

CREATE TABLE policy_resolution_watermark (
    singleton  BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    version    BIGINT  NOT NULL
);
INSERT INTO policy_resolution_watermark (singleton, version) VALUES (TRUE, 0);

CREATE FUNCTION bump_policy_resolution_watermark() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE policy_resolution_watermark SET version = version + 1 WHERE singleton;
    RETURN NULL;
END;
$$;

CREATE TRIGGER trg_policy_versions_watermark
    AFTER UPDATE ON policy_versions
    FOR EACH STATEMENT EXECUTE FUNCTION bump_policy_resolution_watermark();

-- ── evaluation evidence (V-001 PDC-I-17, §14.1 step 7, §19.1) ────────────────

CREATE TABLE policy_evaluations (
    evaluation_id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID         NOT NULL,
    -- The caller's decision_request_id; a retry reconciles to this row.
    decision_id               VARCHAR(64)  NOT NULL,
    legal_entity_id           UUID,
    policy_type               VARCHAR(64)  NOT NULL,
    requested_by_principal_id TEXT,
    input_fingerprint         TEXT         NOT NULL,
    action_context            JSONB        NOT NULL DEFAULT '{}',
    as_of                     TIMESTAMPTZ  NOT NULL,
    known_at                  TIMESTAMPTZ  NOT NULL,
    result                    VARCHAR(32)  NOT NULL,
    outcome                   VARCHAR(32)  NOT NULL,
    policy_id                 UUID         NOT NULL,
    policy_version_id         UUID         NOT NULL,
    policy_code               VARCHAR(128) NOT NULL,
    artifact_digest           TEXT         NOT NULL,
    rule_basis                TEXT         NOT NULL,
    selected_rule_ids         TEXT[]       NOT NULL,
    evaluator_version         VARCHAR(64)  NOT NULL,
    correlation_id            TEXT         NOT NULL,
    causation_id              TEXT,
    request_id                TEXT,
    evaluated_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, decision_id)
);

ALTER TABLE policy_evaluations ENABLE ROW LEVEL SECURITY;
ALTER TABLE policy_evaluations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON policy_evaluations
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TRIGGER trg_policy_evaluations_append_only
    BEFORE UPDATE OR DELETE ON policy_evaluations
    FOR EACH ROW EXECUTE FUNCTION policy_svc_append_only();
CREATE TRIGGER trg_policy_evaluations_no_truncate
    BEFORE TRUNCATE ON policy_evaluations
    FOR EACH STATEMENT EXECUTE FUNCTION policy_svc_refuse_truncate();

-- Delivery of each evaluation to governance-decision-log-svc. Ids and delivery
-- state only, so the relay can read it across tenants without RLS.
CREATE TABLE policy_evidence_outbox (
    evaluation_id    UUID         PRIMARY KEY REFERENCES policy_evaluations (evaluation_id),
    tenant_id        UUID         NOT NULL,
    attempts         INTEGER      NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_error       TEXT,
    delivered_at     TIMESTAMPTZ,
    -- Set when governance-decision-log-svc refuses the record for good (a
    -- conflicting decision_id); such a row is alerted on, never retried.
    failed_at        TIMESTAMPTZ,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_policy_evidence_outbox_pending
    ON policy_evidence_outbox (next_attempt_at)
    WHERE delivered_at IS NULL AND failed_at IS NULL;

-- ── event outbox: stable event ids, exact payload bytes ──────────────────────

ALTER TABLE outbox
    ADD COLUMN event_id          UUID        NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN aggregate_id      TEXT,
    ADD COLUMN aggregate_version INTEGER,
    ADD COLUMN causation_id      TEXT,
    ADD COLUMN next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- JSONB re-orders keys, so a payload_hash over the bytes read back could never
-- be verified by a consumer. BYTEA keeps the exact bytes that were hashed.
ALTER TABLE outbox ALTER COLUMN payload TYPE BYTEA USING convert_to(payload::text, 'UTF8');

-- ── control tests and attestations are tenant data ───────────────────────────

ALTER TABLE control_test_definitions ADD COLUMN tenant_id UUID;
ALTER TABLE control_test_executions  ADD COLUMN tenant_id UUID;
ALTER TABLE attestations             ADD COLUMN tenant_id UUID;

DROP INDEX IF EXISTS idx_control_test_definitions_code_unique;
CREATE UNIQUE INDEX idx_control_test_definitions_code_per_owner
    ON control_test_definitions (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::UUID), test_code);

-- A definition with no tenant is a platform catalogue entry, readable by all.
ALTER TABLE control_test_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE control_test_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON control_test_definitions
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- Executions and attestations are evidence about one tenant's controls. A
-- legacy row with no tenant is readable by no one until it is assigned one:
-- failing closed is the only safe reading of an unscoped evidence record.
ALTER TABLE control_test_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE control_test_executions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON control_test_executions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE attestations ENABLE ROW LEVEL SECURITY;
ALTER TABLE attestations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON attestations
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
