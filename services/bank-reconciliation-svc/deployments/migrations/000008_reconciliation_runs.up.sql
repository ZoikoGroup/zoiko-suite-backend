-- BNK-05: Durable reconciliation run entity with lifecycle.
-- Implements the spec's requirement for a run with frozen population,
-- bound policy, and certification.

CREATE TABLE reconciliation_runs (
    run_id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    legal_entity_id          UUID NOT NULL,
    bank_account_id          UUID NOT NULL,
    statement_date           DATE NOT NULL,
    status                   VARCHAR(32) NOT NULL DEFAULT 'DRAFT',
    -- policy binding (resolved at run start, never changes after RUNNING)
    policy_id                UUID,
    policy_version           INT,
    max_unmatched_count      INT NOT NULL DEFAULT 0,
    max_unmatched_pct        NUMERIC(7,4) NOT NULL DEFAULT 0,
    -- population snapshot reference (set when population is frozen)
    population_id            UUID,
    population_version       INT NOT NULL DEFAULT 0,
    -- certification
    certified_by_principal_id VARCHAR(255),
    certified_at             TIMESTAMPTZ,
    -- lineage
    superseded_by_run_id     UUID REFERENCES reconciliation_runs(run_id),
    prior_run_id             UUID REFERENCES reconciliation_runs(run_id),
    -- audit
    created_by_principal_id  VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    correlation_id           VARCHAR(255) NOT NULL DEFAULT ''
);

CREATE INDEX idx_reconciliation_runs_tenant_account_date
    ON reconciliation_runs (tenant_id, bank_account_id, statement_date);
CREATE INDEX idx_reconciliation_runs_status
    ON reconciliation_runs (tenant_id, status);

ALTER TABLE reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reconciliation_runs
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

-- Tolerance/materiality policies — versioned and immutable once created.
CREATE TABLE reconciliation_policies (
    policy_id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    legal_entity_id          UUID NOT NULL,
    policy_version           INT NOT NULL,
    max_unmatched_count      INT NOT NULL DEFAULT 0,
    max_unmatched_pct        NUMERIC(7,4) NOT NULL DEFAULT 0,
    max_unresolved_amount    NUMERIC(18,4) NOT NULL DEFAULT 0,
    currency                 VARCHAR(16) NOT NULL DEFAULT 'USD',
    rationale                TEXT NOT NULL DEFAULT '',
    effective_from           DATE NOT NULL,
    effective_to             DATE,
    created_by_principal_id  VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_reconciliation_policies_version
    ON reconciliation_policies (tenant_id, legal_entity_id, policy_version);

ALTER TABLE reconciliation_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reconciliation_policies
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

-- Policies are append-only evidence.
CREATE OR REPLACE FUNCTION reject_policy_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation_policies rows are append-only and can never be updated or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_policy_mutation
    BEFORE UPDATE OR DELETE ON reconciliation_policies
    FOR EACH ROW EXECUTE FUNCTION reject_policy_mutation();

-- Population snapshots.
CREATE TABLE reconciliation_populations (
    population_id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id                   UUID NOT NULL REFERENCES reconciliation_runs(run_id),
    tenant_id                UUID NOT NULL,
    legal_entity_id          UUID NOT NULL,
    bank_account_id          UUID NOT NULL,
    statement_date           DATE NOT NULL,
    snapshot_version         INT NOT NULL DEFAULT 1,
    line_count               INT NOT NULL DEFAULT 0,
    total_amount_cents       BIGINT NOT NULL DEFAULT 0,
    bank_population_hash     VARCHAR(64) NOT NULL DEFAULT '',
    frozen_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    frozen_by_principal_id   VARCHAR(255) NOT NULL,
    source_watermark         TIMESTAMPTZ,
    policy_id                UUID REFERENCES reconciliation_policies(policy_id),
    correlation_id           VARCHAR(255) NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX idx_reconciliation_populations_run
    ON reconciliation_populations (run_id);
CREATE INDEX idx_reconciliation_populations_tenant_account_date
    ON reconciliation_populations (tenant_id, bank_account_id, statement_date);

ALTER TABLE reconciliation_populations ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_populations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reconciliation_populations
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

CREATE OR REPLACE FUNCTION reject_population_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation_populations rows are append-only and can never be updated or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_population_mutation
    BEFORE UPDATE OR DELETE ON reconciliation_populations
    FOR EACH ROW EXECUTE FUNCTION reject_population_mutation();

-- Population line items (snapshot of each statement line at freeze time).
CREATE TABLE reconciliation_population_lines (
    id                       BIGSERIAL PRIMARY KEY,
    population_id            UUID NOT NULL REFERENCES reconciliation_populations(population_id),
    tenant_id                UUID NOT NULL,
    statement_line_id        UUID NOT NULL,
    source_system            VARCHAR(64) NOT NULL DEFAULT 'bank-reconciliation-svc',
    source_record_id         VARCHAR(255) NOT NULL,
    transaction_date         DATE NOT NULL,
    amount_cents             BIGINT NOT NULL,
    currency                 VARCHAR(16) NOT NULL,
    bank_reference           VARCHAR(255) NOT NULL DEFAULT '',
    line_status              VARCHAR(32) NOT NULL,
    included                 BOOLEAN NOT NULL DEFAULT TRUE,
    row_hash                 VARCHAR(64) NOT NULL DEFAULT '',
    source_watermark         TIMESTAMPTZ
);

CREATE INDEX idx_population_lines_population ON reconciliation_population_lines (population_id);
CREATE INDEX idx_population_lines_stmt_line ON reconciliation_population_lines (statement_line_id);

ALTER TABLE reconciliation_population_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_population_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON reconciliation_population_lines
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

CREATE OR REPLACE FUNCTION reject_population_line_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation_population_lines rows are append-only evidence';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_population_line_mutation
    BEFORE UPDATE OR DELETE ON reconciliation_population_lines
    FOR EACH ROW EXECUTE FUNCTION reject_population_line_mutation();

-- Update reconciliation_certificates to reference run and population.
ALTER TABLE reconciliation_certificates
    ADD COLUMN run_id        UUID REFERENCES reconciliation_runs(run_id),
    ADD COLUMN population_id UUID REFERENCES reconciliation_populations(population_id),
    ADD COLUMN population_version INT NOT NULL DEFAULT 1;

-- Allow multiple certificates for the same account+date (different runs).
-- Drop the old single-certificate-per-statement constraint.
DROP INDEX IF EXISTS idx_reconciliation_certificates_statement;

-- New: idempotency is per run (not per account+date).
CREATE UNIQUE INDEX idx_reconciliation_certificates_run
    ON reconciliation_certificates (run_id) WHERE run_id IS NOT NULL;
-- Keep backward-compat index for pre-run certificates (run_id IS NULL).
CREATE UNIQUE INDEX idx_reconciliation_certificates_legacy
    ON reconciliation_certificates (tenant_id, bank_account_id, statement_date)
    WHERE run_id IS NULL;
