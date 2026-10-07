-- Schema for data-quality-svc (DATA-02, ZS-SVC-N-001 §4)
-- Migration: 000001_initial_schema.up.sql

-- Rule Sets: the named, stable identity of a rule collection.
CREATE TABLE IF NOT EXISTS dq_rule_sets (
    ruleset_id           TEXT         PRIMARY KEY
        CHECK (ruleset_id ~ '^dqs_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id              VARCHAR(64)  NOT NULL,
    name                     VARCHAR(128) NOT NULL,
    created_at                 TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                   VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, name)
);

-- Rule Set Versions: the actual versioned content. Publishing a modified
-- rule set always creates a NEW version row — an existing published
-- version is never edited in place (doc's own named acceptance test).
CREATE TABLE IF NOT EXISTS dq_rule_set_versions (
    version_id               TEXT         PRIMARY KEY
        CHECK (version_id ~ '^dqv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                  VARCHAR(64)  NOT NULL,
    ruleset_id                   TEXT         NOT NULL REFERENCES dq_rule_sets(ruleset_id),
    version_number                  INT          NOT NULL CHECK (version_number >= 1),
    rules                             JSONB        NOT NULL,
    published_at                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_by                         VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, ruleset_id, version_number)
);

CREATE INDEX IF NOT EXISTS idx_dq_rule_set_versions_ruleset ON dq_rule_set_versions(tenant_id, ruleset_id);

-- DQ Runs: one evaluation attempt against a FROZEN, caller-supplied
-- population — this service trusts and records that evidence, it never
-- recomputes it (same "frozen populations" doctrine as DATA-07).
CREATE TABLE IF NOT EXISTS dq_runs (
    run_id                     TEXT         PRIMARY KEY
        CHECK (run_id ~ '^dqn_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                    VARCHAR(64)  NOT NULL,
    ruleset_version_id             TEXT         NOT NULL REFERENCES dq_rule_set_versions(version_id),
    population_ref                    VARCHAR(256) NOT NULL,
    population_row_count                 BIGINT       NOT NULL CHECK (population_row_count > 0),
    population_content_hash                 CHAR(64)     NOT NULL CHECK (population_content_hash ~ '^[0-9a-f]{64}$'),
    status                                     VARCHAR(32)  NOT NULL,
    supersedes_run_id                             TEXT         REFERENCES dq_runs(run_id),
    started_at                                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at                                       TIMESTAMPTZ,
    created_by                                           VARCHAR(128) NOT NULL,
    CHECK (status IN ('Planned', 'Running', 'ExceptionsOpen', 'Reperformed', 'Certified', 'Failed'))
);

CREATE INDEX IF NOT EXISTS idx_dq_runs_tenant_status ON dq_runs(tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_dq_runs_ruleset_version ON dq_runs(tenant_id, ruleset_version_id);

-- DQ Results: one rule's outcome for one run — part of the run's frozen
-- evidence, append-only.
CREATE TABLE IF NOT EXISTS dq_results (
    result_id                 TEXT         PRIMARY KEY
        CHECK (result_id ~ '^dqr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                    VARCHAR(64)  NOT NULL,
    run_id                          TEXT         NOT NULL REFERENCES dq_runs(run_id),
    rule_key                          VARCHAR(128) NOT NULL,
    status                              VARCHAR(16)  NOT NULL CHECK (status IN ('Pass', 'Fail')),
    pass_count                            BIGINT       NOT NULL DEFAULT 0 CHECK (pass_count >= 0),
    fail_count                              BIGINT       NOT NULL DEFAULT 0 CHECK (fail_count >= 0),
    failure_magnitude_sum                      NUMERIC      NOT NULL DEFAULT 0 CHECK (failure_magnitude_sum >= 0),
    failing_record_refs                          JSONB        NOT NULL DEFAULT '[]'::jsonb,
    evaluated_at                                    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, rule_key)
);

CREATE INDEX IF NOT EXISTS idx_dq_results_run ON dq_results(tenant_id, run_id);
CREATE INDEX IF NOT EXISTS idx_dq_results_failing ON dq_results(tenant_id, run_id) WHERE status = 'Fail';

-- DQ Issues: a failing result requiring disposition. Deliberately no
-- command marks one resolved directly — the only path to clearing an
-- issue is a Reperform whose new run passes (doc's own named acceptance
-- test). AssignIssue is the ONE allowed transition: Open -> Assigned.
CREATE TABLE IF NOT EXISTS dq_issues (
    issue_id                 TEXT         PRIMARY KEY
        CHECK (issue_id ~ '^dqi_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                  VARCHAR(64)  NOT NULL,
    run_id                        TEXT         NOT NULL REFERENCES dq_runs(run_id),
    result_id                       TEXT         NOT NULL REFERENCES dq_results(result_id),
    description                       TEXT         NOT NULL,
    status                               VARCHAR(16)  NOT NULL DEFAULT 'Open' CHECK (status IN ('Open', 'Assigned')),
    assignee                               VARCHAR(128),
    raised_at                                 TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    raised_by                                   VARCHAR(128) NOT NULL,
    CHECK ((status = 'Assigned') = (assignee IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_dq_issues_run ON dq_issues(tenant_id, run_id);
CREATE INDEX IF NOT EXISTS idx_dq_issues_open ON dq_issues(tenant_id, run_id) WHERE status = 'Open';

-- DQ Certifications: the sealed, immutable outcome of a run once it has
-- no open issues — same "sealed evidence, sha256, immutable at the
-- database" doctrine as data-lineage-svc's ProvenanceManifest.
CREATE TABLE IF NOT EXISTS dq_certifications (
    certification_id         TEXT         PRIMARY KEY
        CHECK (certification_id ~ '^dqc_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                   VARCHAR(64)  NOT NULL,
    run_id                         TEXT         NOT NULL UNIQUE REFERENCES dq_runs(run_id),
    summary                           JSONB        NOT NULL,
    summary_sha256                       CHAR(64)     NOT NULL CHECK (summary_sha256 ~ '^[0-9a-f]{64}$'),
    certified_at                            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    certified_by                               VARCHAR(128) NOT NULL
);

-- Idempotency Keys: scoped per tenant from the start.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(64)  NOT NULL,
    owner_scope           VARCHAR(32)  NOT NULL,
    principal_id             VARCHAR(128) NOT NULL,
    idempotency_key            VARCHAR(128) NOT NULL,
    operation                    VARCHAR(64)  NOT NULL,
    request_sha256                 VARCHAR(64)  NOT NULL,
    resource_id                      VARCHAR(128) NOT NULL,
    created_at                         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- Outbox Events: transactional outbox for event emission.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type        VARCHAR(64)  NOT NULL,
    aggregate_id            VARCHAR(128) NOT NULL,
    event_type                VARCHAR(64)  NOT NULL,
    payload                      JSONB        NOT NULL,
    tenant_id                      VARCHAR(64),
    created_at                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at                       TIMESTAMPTZ,
    publish_attempts                     INT          NOT NULL DEFAULT 0,
    last_error                             TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events(published_at) WHERE published_at IS NULL;

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE dq_rule_sets ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_rule_sets FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_rule_sets_tenant_isolation ON dq_rule_sets
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dq_rule_set_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_rule_set_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_rule_set_versions_read ON dq_rule_set_versions FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dq_rule_set_versions_insert ON dq_rule_set_versions FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dq_rule_set_versions_update ON dq_rule_set_versions FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dq_rule_set_versions_delete ON dq_rule_set_versions FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dq_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_runs_tenant_isolation ON dq_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dq_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_results FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_results_read ON dq_results FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dq_results_insert ON dq_results FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dq_results_update ON dq_results FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dq_results_delete ON dq_results FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dq_issues ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_issues FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_issues_tenant_isolation ON dq_issues
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dq_certifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_certifications FORCE ROW LEVEL SECURITY;
CREATE POLICY dq_certifications_read ON dq_certifications FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dq_certifications_insert ON dq_certifications FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dq_certifications_update ON dq_certifications FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY dq_certifications_delete ON dq_certifications FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- ── Immutability ─────────────────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION enforce_dq_rule_set_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dq rule set is immutable once created; publish a new version instead';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dq_rule_set_immutability
    BEFORE UPDATE OR DELETE ON dq_rule_sets
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_rule_set_immutability();

CREATE OR REPLACE FUNCTION enforce_dq_rule_set_version_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dq rule set version is immutable once published';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dq_rule_set_version_immutability
    BEFORE UPDATE OR DELETE ON dq_rule_set_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_rule_set_version_immutability();

CREATE OR REPLACE FUNCTION enforce_dq_result_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dq result is immutable once evaluated';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dq_result_immutability
    BEFORE UPDATE OR DELETE ON dq_results
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_result_immutability();

CREATE OR REPLACE FUNCTION enforce_dq_certification_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dq certification is sealed and immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dq_certification_immutability
    BEFORE UPDATE OR DELETE ON dq_certifications
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_certification_immutability();

-- DQ Issues: the ONLY allowed mutation is Open -> Assigned, exactly once.
-- No other transition exists (no "resolve" path — see the doc comment on
-- domain.DQIssue for why that's deliberate, not an oversight).
CREATE OR REPLACE FUNCTION enforce_dq_issue_assign_only()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dq issue cannot be deleted';
    END IF;
    IF OLD.status <> 'Open' THEN
        RAISE EXCEPTION 'dq issue % is already %, and issues are never mutated beyond Open -> Assigned', OLD.issue_id, OLD.status;
    END IF;
    IF NEW.status <> 'Assigned' THEN
        RAISE EXCEPTION 'dq issue may only transition from Open to Assigned';
    END IF;
    IF NEW.run_id <> OLD.run_id OR NEW.result_id <> OLD.result_id OR NEW.description <> OLD.description
        OR NEW.raised_at <> OLD.raised_at OR NEW.raised_by <> OLD.raised_by THEN
        RAISE EXCEPTION 'dq issue assignment may only change status/assignee';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dq_issue_assign_only
    BEFORE UPDATE OR DELETE ON dq_issues
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_issue_assign_only();

-- DQ Runs: a terminal run (Certified/Reperformed/Failed) is immutable
-- outright, not just its status column.
CREATE OR REPLACE FUNCTION enforce_dq_run_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dq run cannot be deleted';
    END IF;
    IF OLD.status IN ('Certified', 'Reperformed', 'Failed') THEN
        RAISE EXCEPTION 'dq run % is terminal (%) and immutable', OLD.run_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Running' THEN
                IF NEW.status NOT IN ('ExceptionsOpen', 'Certified', 'Reperformed', 'Failed') THEN
                    RAISE EXCEPTION 'invalid dq run transition from Running to %', NEW.status;
                END IF;
            WHEN 'ExceptionsOpen' THEN
                IF NEW.status NOT IN ('Reperformed', 'Failed') THEN
                    RAISE EXCEPTION 'invalid dq run transition from ExceptionsOpen to %; certify requires a clean Reperform first', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown dq run status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_dq_run_lifecycle
    BEFORE UPDATE OR DELETE ON dq_runs
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_run_lifecycle();
