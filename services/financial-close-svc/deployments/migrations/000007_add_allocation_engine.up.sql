-- Migration: 000007_add_allocation_engine.up.sql
--
-- ACC-09 (Allocation Engine): "owns Allocation rules/runs. Must never own:
-- Source population or ledger truth." Fuller ownership: "AllocationRuleVersion,
-- AllocationRun, driver snapshot, result lines and calculation evidence."
--
-- allocation_rules is effective-dated version rows under a stable logical
-- rule_id — the same doctrine as ACC-02's account_mappings (migration
-- 000008 in general-ledger-svc): a partial unique index on
-- (rule_id) WHERE effective_to IS NULL makes "at most one CURRENT version
-- per logical rule" database-enforced. This is deliberate, not
-- decorative: the spec's own negative path "Driver changes after run
-- starts" requires that drivers never mutate in place once approved — the
-- only way to change them is to create a NEW version, which supersedes
-- the old one going forward while every past run keeps pointing at the
-- exact rule_version_id it actually ran under.
--
-- allocation_runs is a normal mutable stateful row (PLANNED -> CALCULATED
-- -> POSTED/FAILED), UNIQUE per (rule_id, fiscal_period) — "Rerun
-- duplicates posting" is blocked by construction: a rule can produce at
-- most one run per period, ever, and once POSTED it is never re-posted.
--
-- allocation_run_result_lines and allocation_run_driver_snapshot are
-- append-only calculation evidence — the run's own permanent record of
-- what it computed and why, per the spec's own evidence requirement:
-- "Persist source watermark, driver values, rule version, rounding and
-- posting refs."

CREATE TABLE allocation_rules (
    rule_version_id       UUID PRIMARY KEY,
    rule_id                UUID NOT NULL, -- stable logical identity across versions
    version                 INT NOT NULL,
    tenant_id                VARCHAR(255) NOT NULL,
    legal_entity_id          VARCHAR(255) NOT NULL,
    name                      TEXT NOT NULL,
    source_account_code       VARCHAR(64) NOT NULL,
    status                    VARCHAR(20) NOT NULL, -- DRAFT|APPROVED|ACTIVE|SUPERSEDED
    created_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id   VARCHAR(255) NOT NULL,
    approved_at               TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id  VARCHAR(255),
    effective_to              TIMESTAMP WITH TIME ZONE -- NULL = current version
);

CREATE UNIQUE INDEX idx_allocation_rules_current_version
    ON allocation_rules (rule_id) WHERE effective_to IS NULL;

CREATE TABLE allocation_rule_drivers (
    rule_version_id        UUID NOT NULL REFERENCES allocation_rules(rule_version_id),
    recipient_account_code   VARCHAR(64) NOT NULL,
    weight_percentage        NUMERIC(7,4) NOT NULL -- e.g. 33.3333; all drivers for a version must sum to 100.0000
);

CREATE TABLE allocation_runs (
    run_id                 UUID PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id          VARCHAR(255) NOT NULL,
    rule_id                   UUID NOT NULL,
    rule_version_id           UUID NOT NULL REFERENCES allocation_rules(rule_version_id),
    fiscal_period              VARCHAR(20) NOT NULL,
    source_account_code        VARCHAR(64) NOT NULL,
    source_amount               NUMERIC(18,2) NOT NULL,
    status                      VARCHAR(20) NOT NULL, -- PLANNED|CALCULATED|POSTED|FAILED
    journal_id                  VARCHAR(255),
    failure_reason               TEXT,
    created_at                   TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id      VARCHAR(255) NOT NULL,
    calculated_at                 TIMESTAMP WITH TIME ZONE,
    posted_at                     TIMESTAMP WITH TIME ZONE,
    UNIQUE (rule_id, fiscal_period)
);

CREATE TABLE allocation_run_result_lines (
    result_line_id         UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    run_id                    UUID NOT NULL REFERENCES allocation_runs(run_id),
    recipient_account_code     VARCHAR(64) NOT NULL,
    allocated_amount            NUMERIC(18,2) NOT NULL
);

CREATE OR REPLACE FUNCTION reject_allocation_result_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'allocation_run_result_lines is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_allocation_result_update
    BEFORE UPDATE ON allocation_run_result_lines
    FOR EACH ROW EXECUTE FUNCTION reject_allocation_result_mutation();
CREATE TRIGGER trg_reject_allocation_result_delete
    BEFORE DELETE ON allocation_run_result_lines
    FOR EACH ROW EXECUTE FUNCTION reject_allocation_result_mutation();

ALTER TABLE allocation_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE allocation_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON allocation_rules
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE allocation_rule_drivers ENABLE ROW LEVEL SECURITY;
ALTER TABLE allocation_rule_drivers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON allocation_rule_drivers
    FOR ALL USING (
        rule_version_id IN (SELECT rule_version_id FROM allocation_rules WHERE tenant_id = current_setting('app.tenant_id', true))
    )
    WITH CHECK (
        rule_version_id IN (SELECT rule_version_id FROM allocation_rules WHERE tenant_id = current_setting('app.tenant_id', true))
    );

ALTER TABLE allocation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE allocation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON allocation_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE allocation_run_result_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE allocation_run_result_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON allocation_run_result_lines
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_allocation_rules_entity ON allocation_rules (tenant_id, legal_entity_id);
CREATE INDEX idx_allocation_runs_entity_period ON allocation_runs (tenant_id, legal_entity_id, fiscal_period);
CREATE INDEX idx_allocation_run_result_lines_run ON allocation_run_result_lines (tenant_id, run_id);
