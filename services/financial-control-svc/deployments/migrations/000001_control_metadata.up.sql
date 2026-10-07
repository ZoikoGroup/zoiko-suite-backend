-- ZS-CONTROL-001 Wave 0: control metadata, policies, runs, transition history, outbox.
-- Tenant isolation: RLS (FORCE) AND explicit tenant_id predicates in every store query
-- (this platform connects as a superuser, which bypasses RLS).

CREATE TABLE control_definitions (
    control_definition_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    control_code           VARCHAR(32) NOT NULL,          -- e.g. FIN-CTRL-001
    name                   VARCHAR(255) NOT NULL,
    domain                 VARCHAR(64) NOT NULL,
    control_type           VARCHAR(32) NOT NULL CHECK (control_type IN
        ('BALANCE','TRANSACTION','INTERFACE','REPORTING','EXTERNAL','PREVENTIVE','MONITORING','REVIEW','ENTITY_LEVEL')),
    assertions             TEXT[] NOT NULL CHECK (cardinality(assertions) > 0),
    risk_tier              VARCHAR(16) NOT NULL CHECK (risk_tier IN ('KEY','STANDARD','LOW')),
    frequency              VARCHAR(32) NOT NULL CHECK (frequency IN
        ('EVENT_DRIVEN','INTRADAY','DAILY','PERIOD_END','ON_DEMAND','PER_BATCH','CONTINUOUS')),
    close_gating           BOOLEAN NOT NULL DEFAULT FALSE, -- mandatory close gate when classified key control
    scope                  JSONB NOT NULL DEFAULT '{}',    -- entity/book/currency/jurisdiction
    owner_role             VARCHAR(128) NOT NULL,
    reviewer_role          VARCHAR(128) NOT NULL DEFAULT '',
    certifier_role         VARCHAR(128) NOT NULL DEFAULT '',
    source_spec            JSONB NOT NULL DEFAULT '{}',    -- source (side A) population definition
    target_spec            JSONB NOT NULL DEFAULT '{}',    -- target (side B) / expected condition
    evidence_policy        JSONB NOT NULL DEFAULT '{}',
    policy_refs            TEXT[] NOT NULL DEFAULT '{}',
    created_by             VARCHAR(255) NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    correlation_id         VARCHAR(255) NOT NULL DEFAULT ''
);
-- One stable id per (tenant, code); logic changes are new rule versions, never mutations.
CREATE UNIQUE INDEX uq_control_definition_code ON control_definitions (tenant_id, control_code);

CREATE TABLE control_rule_versions (
    rule_version_id        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    control_definition_id  UUID NOT NULL REFERENCES control_definitions(control_definition_id),
    rule_version           INT NOT NULL CHECK (rule_version > 0),
    logic_digest           VARCHAR(71) NOT NULL,           -- sha256:<hex> of rule/config
    logic                  JSONB NOT NULL DEFAULT '{}',
    test_pack_version      VARCHAR(64) NOT NULL DEFAULT '',
    effective_from         DATE NOT NULL,
    effective_to           DATE,
    approved_by            VARCHAR(255),                   -- independent approval for key controls
    approved_at            TIMESTAMPTZ,
    created_by             VARCHAR(255) NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (approved_by IS NULL OR approved_by <> created_by)
);
CREATE UNIQUE INDEX uq_control_rule_version ON control_rule_versions (tenant_id, control_definition_id, rule_version);

CREATE TABLE tolerance_policies (
    tolerance_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    legal_entity_id        UUID NOT NULL,
    metric                 VARCHAR(64) NOT NULL,
    tolerance_version      INT NOT NULL CHECK (tolerance_version > 0),
    absolute_tolerance     NUMERIC(38,12) NOT NULL DEFAULT 0 CHECK (absolute_tolerance >= 0),
    percentage_tolerance   NUMERIC(9,6)  NOT NULL DEFAULT 0 CHECK (percentage_tolerance >= 0),
    percentage_base        VARCHAR(32) NOT NULL DEFAULT '',  -- explicit base required when percentage > 0
    date_tolerance_days    INT NOT NULL DEFAULT 0 CHECK (date_tolerance_days >= 0),
    date_basis             VARCHAR(16) NOT NULL DEFAULT 'CALENDAR' CHECK (date_basis IN ('CALENDAR','BUSINESS')),
    currency               VARCHAR(3) NOT NULL,
    permitted_contexts     TEXT[] NOT NULL DEFAULT '{}',
    rationale              TEXT NOT NULL,
    effective_from         DATE NOT NULL,
    approved_by            VARCHAR(255) NOT NULL,
    created_by             VARCHAR(255) NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (percentage_tolerance = 0 OR percentage_base <> ''),
    CHECK (approved_by <> created_by)
);
CREATE UNIQUE INDEX uq_tolerance_version ON tolerance_policies (tenant_id, legal_entity_id, metric, tolerance_version);

CREATE TABLE materiality_policies (
    materiality_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    legal_entity_id        UUID NOT NULL,
    reporting_basis        VARCHAR(64) NOT NULL,
    materiality_version    INT NOT NULL CHECK (materiality_version > 0),
    amount_threshold       NUMERIC(38,12) NOT NULL CHECK (amount_threshold >= 0),
    aggregate_threshold    NUMERIC(38,12) NOT NULL CHECK (aggregate_threshold >= 0),
    currency               VARCHAR(3) NOT NULL,
    qualitative_triggers   TEXT[] NOT NULL DEFAULT '{}',
    aggregation_basis      VARCHAR(64) NOT NULL DEFAULT 'ENTITY_PERIOD',
    effective_from         DATE NOT NULL,
    approved_by            VARCHAR(255) NOT NULL,
    created_by             VARCHAR(255) NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (approved_by <> created_by)
);
CREATE UNIQUE INDEX uq_materiality_version ON materiality_policies (tenant_id, legal_entity_id, reporting_basis, materiality_version);

-- Control runs. lifecycle_state / result_state / certification_state are ORTHOGONAL (§7).
CREATE TABLE control_runs (
    run_id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    legal_entity_id        UUID NOT NULL,
    control_definition_id  UUID NOT NULL REFERENCES control_definitions(control_definition_id),
    rule_version           INT NOT NULL,                    -- pinned at creation
    rule_digest            VARCHAR(71) NOT NULL,            -- pinned at creation
    tolerance_id           UUID REFERENCES tolerance_policies(tolerance_id),   -- pinned, never widened at run time
    materiality_id         UUID REFERENCES materiality_policies(materiality_id),
    period_id              VARCHAR(32) NOT NULL DEFAULT '',
    scope                  JSONB NOT NULL DEFAULT '{}',
    trigger_type           VARCHAR(16) NOT NULL CHECK (trigger_type IN
        ('EVENT','INTRADAY','DAILY','PERIOD_END','ON_DEMAND')),
    trigger_reason         TEXT NOT NULL DEFAULT '',
    prior_run_id           UUID REFERENCES control_runs(run_id),
    superseded_by_run_id   UUID REFERENCES control_runs(run_id),
    lifecycle_state        VARCHAR(24) NOT NULL DEFAULT 'SCHEDULED' CHECK (lifecycle_state IN
        ('SCHEDULED','PREPARING','POPULATION_FROZEN','EXECUTING','EXCEPTION_REVIEW','REMEDIATION',
         'REPERFORMANCE','READY_TO_CERTIFY','CERTIFIED','FAILED','EXPIRED','SUPERSEDED')),
    result_state           VARCHAR(32) NOT NULL DEFAULT 'NOT_EVALUATED' CHECK (result_state IN
        ('NOT_EVALUATED','PASS','PASS_WITH_APPROVED_EXCEPTIONS','FAIL','INDETERMINATE')),
    certification_state    VARCHAR(16) NOT NULL DEFAULT 'NOT_REQUIRED' CHECK (certification_state IN
        ('NOT_REQUIRED','PENDING','CERTIFIED','REJECTED','REVOKED','SUPERSEDED')),
    version                INT NOT NULL DEFAULT 1,          -- optimistic concurrency (ETag)
    started_at             TIMESTAMPTZ,
    completed_at           TIMESTAMPTZ,
    created_by             VARCHAR(255) NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    idempotency_key        VARCHAR(255) NOT NULL,
    correlation_id         VARCHAR(255) NOT NULL DEFAULT '',
    -- Invariants 5/9: certification only over a passing result; a technical failure is never a Pass.
    CHECK (lifecycle_state <> 'CERTIFIED' OR certification_state = 'CERTIFIED'),
    CHECK (certification_state <> 'CERTIFIED' OR result_state IN ('PASS','PASS_WITH_APPROVED_EXCEPTIONS')),
    CHECK (lifecycle_state <> 'FAILED' OR result_state IN ('FAIL','INDETERMINATE','NOT_EVALUATED'))
);
CREATE UNIQUE INDEX uq_control_run_idempotency ON control_runs (tenant_id, idempotency_key);
CREATE INDEX idx_control_runs_definition ON control_runs (tenant_id, control_definition_id, created_at DESC);
CREATE INDEX idx_control_runs_period ON control_runs (tenant_id, legal_entity_id, period_id);

-- Append-only transition history: nothing is silently rewritten (Invariant 13).
CREATE TABLE control_run_transitions (
    transition_id          BIGSERIAL PRIMARY KEY,
    tenant_id              UUID NOT NULL,
    run_id                 UUID NOT NULL REFERENCES control_runs(run_id),
    dimension              VARCHAR(16) NOT NULL CHECK (dimension IN ('LIFECYCLE','RESULT','CERTIFICATION')),
    from_state             VARCHAR(32) NOT NULL,
    to_state               VARCHAR(32) NOT NULL,
    reason                 TEXT NOT NULL DEFAULT '',
    actor_id               VARCHAR(255) NOT NULL,
    correlation_id         VARCHAR(255) NOT NULL DEFAULT '',
    occurred_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_control_run_transitions_run ON control_run_transitions (tenant_id, run_id, transition_id);

CREATE TABLE outbox_events (
    outbox_event_id      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type       VARCHAR(64) NOT NULL,
    aggregate_id         VARCHAR(128) NOT NULL,
    event_type           VARCHAR(128) NOT NULL,
    tenant_id            UUID        NOT NULL,
    legal_entity_id      UUID        NOT NULL,
    actor_id             TEXT,
    correlation_id       TEXT,
    headers              JSONB,
    payload              JSONB       NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at         TIMESTAMPTZ,
    publish_attempts     INTEGER     NOT NULL DEFAULT 0,
    last_error           TEXT
);
CREATE INDEX idx_outbox_events_unpublished ON outbox_events (created_at ASC) WHERE published_at IS NULL;

-- Append-only guards (Invariants 2, 4, 13): definitions, policies and history are immutable.
CREATE OR REPLACE FUNCTION reject_control_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% rows are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_immutable_definitions BEFORE UPDATE OR DELETE ON control_definitions
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();
CREATE TRIGGER trg_immutable_tolerance BEFORE UPDATE OR DELETE ON tolerance_policies
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();
CREATE TRIGGER trg_immutable_materiality BEFORE UPDATE OR DELETE ON materiality_policies
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();
CREATE TRIGGER trg_immutable_transitions BEFORE UPDATE OR DELETE ON control_run_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();

-- Rule versions: only the approval columns may be set, once, and never deleted.
CREATE OR REPLACE FUNCTION guard_rule_version() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'control_rule_versions rows can never be deleted';
    END IF;
    IF OLD.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'approved control_rule_versions rows are immutable';
    END IF;
    IF NEW.rule_version_id <> OLD.rule_version_id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.control_definition_id <> OLD.control_definition_id OR NEW.rule_version <> OLD.rule_version
       OR NEW.logic_digest <> OLD.logic_digest OR NEW.logic::text <> OLD.logic::text
       OR NEW.effective_from <> OLD.effective_from OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'only approval columns of control_rule_versions may change';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_guard_rule_version BEFORE UPDATE OR DELETE ON control_rule_versions
    FOR EACH ROW EXECUTE FUNCTION guard_rule_version();

-- RLS on every tenant table. outbox_events is deliberately excluded: the relay
-- polls it across all tenants (FOR UPDATE SKIP LOCKED) with no tenant scope set,
-- exactly like workflow-svc's outbox; it is written only inside domain transactions.
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['control_definitions','control_rule_versions','tolerance_policies',
        'materiality_policies','control_runs','control_run_transitions']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation_policy ON %I FOR ALL '
            'USING (tenant_id = current_setting(''app.tenant_id'', true)::UUID) '
            'WITH CHECK (tenant_id = current_setting(''app.tenant_id'', true)::UUID)', t);
    END LOOP;
END $$;
