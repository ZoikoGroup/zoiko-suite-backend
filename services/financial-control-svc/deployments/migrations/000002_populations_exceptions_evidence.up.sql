-- ZS-CONTROL-001 Wave 1: frozen populations, match results, exceptions, sealed evidence.

-- A frozen population snapshot: what exactly was tested (§9). One per side per run.
CREATE TABLE population_snapshots (
    population_id    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL,
    run_id           UUID NOT NULL REFERENCES control_runs(run_id),
    side             CHAR(1) NOT NULL CHECK (side IN ('A','B')),
    source_system    VARCHAR(64) NOT NULL,
    spec_ref         VARCHAR(160) NOT NULL,
    row_count        INT NOT NULL CHECK (row_count >= 0),
    control_totals   JSONB NOT NULL DEFAULT '[]',
    population_hash  VARCHAR(71) NOT NULL CHECK (population_hash ~ '^sha256:[0-9a-f]{64}$'),
    source_watermark VARCHAR(255) NOT NULL CHECK (source_watermark <> ''),   -- Invariant 3
    exclusions       JSONB NOT NULL DEFAULT '[]',
    frozen_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_population_run_side ON population_snapshots (run_id, side);
CREATE INDEX idx_population_tenant_run ON population_snapshots (tenant_id, run_id);

-- Record identity set. Deliberately NO unique (population_id, record_id): a source that
-- delivers one id twice must be able to record BOTH so the duplicate is provable.
CREATE TABLE population_records (
    seq           BIGSERIAL PRIMARY KEY,
    tenant_id     UUID NOT NULL,
    population_id UUID NOT NULL REFERENCES population_snapshots(population_id),
    record_id     VARCHAR(255) NOT NULL,
    reference     VARCHAR(255) NOT NULL,
    amount        NUMERIC(38,12) NOT NULL,
    currency      CHAR(3) NOT NULL,
    record_date   DATE NOT NULL,
    attributes    JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_population_records_pop ON population_records (population_id, seq);
CREATE INDEX idx_population_records_lookup ON population_records (tenant_id, population_id, record_id);

CREATE TABLE match_results (
    match_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    run_id        UUID NOT NULL REFERENCES control_runs(run_id),
    side_a_record VARCHAR(255) NOT NULL,
    side_b_record VARCHAR(255) NOT NULL,
    match_rule    VARCHAR(64) NOT NULL,
    outcome       VARCHAR(24) NOT NULL CHECK (outcome IN ('EXACT','WITHIN_TOLERANCE')),
    difference    NUMERIC(38,12) NOT NULL,
    currency      CHAR(3) NOT NULL
);
CREATE INDEX idx_match_results_run ON match_results (tenant_id, run_id);

-- Control exceptions: authoritative state of every control failure (§21).
CREATE TABLE control_exceptions (
    exception_id       UUID PRIMARY KEY,
    tenant_id          UUID NOT NULL,
    run_id             UUID NOT NULL REFERENCES control_runs(run_id),
    legal_entity_id    UUID NOT NULL,
    category           VARCHAR(24) NOT NULL CHECK (category IN
        ('MISSING','DUPLICATE','MISMATCH','TIMING','VALUATION','CLASSIFICATION','AUTHORIZATION',
         'DATA_QUALITY','EXTERNAL_STATUS','LATE_DATA','CONTROL_EXECUTION_FAILURE')),
    reason_code        VARCHAR(48) NOT NULL,
    assertion          VARCHAR(32) NOT NULL,
    severity           VARCHAR(8) NOT NULL CHECK (severity IN ('LOW','MEDIUM','HIGH')),
    side               CHAR(1) NOT NULL CHECK (side IN ('A','B')),
    record_ids         TEXT[] NOT NULL CHECK (cardinality(record_ids) > 0),
    exposure           NUMERIC(38,12) NOT NULL CHECK (exposure >= 0),
    currency           CHAR(3) NOT NULL,
    detail             TEXT NOT NULL DEFAULT '',
    owner_role         VARCHAR(128) NOT NULL CHECK (owner_role <> ''),    -- Invariant 7: never ownerless
    owner_principal_id VARCHAR(255),
    due_at             TIMESTAMPTZ NOT NULL,                              -- Invariant 7: always has a due date
    state              VARCHAR(40) NOT NULL DEFAULT 'OPEN' CHECK (state IN
        ('OPEN','ASSIGNED','INVESTIGATING','AWAITING_EVIDENCE','AWAITING_ADJUSTMENT','REMEDIATED',
         'REPERFORMED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY','CLOSED')),
    version            INT NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (state = 'OPEN' OR owner_principal_id IS NOT NULL)              -- past OPEN => a named owner
);
CREATE INDEX idx_control_exceptions_run ON control_exceptions (tenant_id, run_id, created_at);
CREATE INDEX idx_control_exceptions_open ON control_exceptions (tenant_id, due_at) WHERE state NOT IN ('CLOSED','WAIVED_UNDER_AUTHORITY','CARRIED_FORWARD_UNDER_AUTHORITY');

CREATE TABLE exception_transitions (
    transition_id  BIGSERIAL PRIMARY KEY,
    tenant_id      UUID NOT NULL,
    exception_id   UUID NOT NULL REFERENCES control_exceptions(exception_id),
    from_state     VARCHAR(40) NOT NULL,
    to_state       VARCHAR(40) NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    actor_id       VARCHAR(255) NOT NULL,
    correlation_id VARCHAR(255) NOT NULL DEFAULT '',
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_exception_transitions ON exception_transitions (tenant_id, exception_id, transition_id);

-- Sealed evidence package: content + digest, never updated (Invariants 13, 16).
CREATE TABLE evidence_packages (
    package_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID NOT NULL,
    run_id     UUID NOT NULL REFERENCES control_runs(run_id),
    content    JSONB NOT NULL,
    digest     VARCHAR(71) NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    algorithm  VARCHAR(64) NOT NULL,
    sealed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    sealed_by  VARCHAR(255) NOT NULL
);
CREATE INDEX idx_evidence_packages_run ON evidence_packages (tenant_id, run_id, sealed_at DESC);

-- Exactly-once command execution (ZS-API-001 idempotency for :execute).
CREATE TABLE command_idempotency (
    tenant_id       UUID NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    command         VARCHAR(32) NOT NULL,
    run_id          UUID NOT NULL REFERENCES control_runs(run_id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

-- Immutability. Population/match/evidence/history rows are never edited or deleted, so a
-- certified population can never be silently mutated (Invariants 2, 13; scenario 05).
CREATE TRIGGER trg_immutable_population_snapshots BEFORE UPDATE OR DELETE ON population_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();
CREATE TRIGGER trg_immutable_population_records BEFORE UPDATE OR DELETE ON population_records
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();
CREATE TRIGGER trg_immutable_match_results BEFORE UPDATE OR DELETE ON match_results
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();
CREATE TRIGGER trg_immutable_exception_transitions BEFORE UPDATE OR DELETE ON exception_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();
CREATE TRIGGER trg_immutable_evidence BEFORE UPDATE OR DELETE ON evidence_packages
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();

-- Exceptions: state/ownership may change, identity and finding may not, and a row can
-- never be deleted to make a control pass (Prohibited anti-pattern: manual deletion).
CREATE OR REPLACE FUNCTION guard_control_exception() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'control_exceptions rows can never be deleted';
    END IF;
    IF NEW.exception_id <> OLD.exception_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.run_id <> OLD.run_id
       OR NEW.category <> OLD.category OR NEW.reason_code <> OLD.reason_code OR NEW.assertion <> OLD.assertion
       OR NEW.side <> OLD.side OR NEW.record_ids <> OLD.record_ids OR NEW.exposure <> OLD.exposure
       OR NEW.currency <> OLD.currency OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the finding recorded on a control exception is immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_guard_control_exception BEFORE UPDATE OR DELETE ON control_exceptions
    FOR EACH ROW EXECUTE FUNCTION guard_control_exception();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['population_snapshots','population_records','match_results','control_exceptions',
        'exception_transitions','evidence_packages','command_idempotency']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY tenant_isolation_policy ON %I FOR ALL '
            'USING (tenant_id = current_setting(''app.tenant_id'', true)::UUID) '
            'WITH CHECK (tenant_id = current_setting(''app.tenant_id'', true)::UUID)', t);
    END LOOP;
END $$;
