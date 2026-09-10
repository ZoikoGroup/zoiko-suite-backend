-- Migration: 000004_add_subledger_control.up.sql
--
-- ACC-06 (Subledger Control): "owns Subledger-to-GL control runs/
-- exceptions. Must never own: underlying subledger/ledger values." Before
-- this migration, what this service called "readiness checks" were just
-- existence-counts (unposted journals, unsettled invoices) — never an
-- actual balance comparison between a subledger total and its GL control
-- account, and no persisted record of a control check ever having run
-- (see master-register-findings-2026-08-27.md's audit of this gap).
--
-- The run and its exception outcome are modeled as ONE row for v1, not
-- two separately-lifecycled objects: the exception IS the run's own
-- outcome (a boolean fact plus the amounts that produced it), and
-- inventing a separate resolution workflow (assigned-to, resolved-by,
-- resolution notes) would be fabricating process the spec doesn't
-- describe, the same discipline already applied elsewhere this session
-- (e.g. ACC-12's elimination fix, ACC-14's reopen path).

CREATE TABLE subledger_control_runs (
    control_run_id           UUID PRIMARY KEY,
    tenant_id                 VARCHAR(255) NOT NULL,
    legal_entity_id           VARCHAR(255) NOT NULL,
    fiscal_period              VARCHAR(20) NOT NULL,
    subledger                  VARCHAR(10) NOT NULL, -- AP | AR
    control_account_code       VARCHAR(64) NOT NULL, -- resolved via ACC-02 mapping at run time
    subledger_total_amount     NUMERIC(18,2) NOT NULL,
    gl_control_balance_amount  NUMERIC(18,2) NOT NULL,
    difference_amount          NUMERIC(18,2) NOT NULL,
    status                     VARCHAR(20) NOT NULL, -- MATCHED | EXCEPTION
    run_at                     TIMESTAMP WITH TIME ZONE NOT NULL,
    run_by_principal_id        VARCHAR(255) NOT NULL
);

-- Append-only: a control run is permanent evidence that a reconciliation
-- check happened and what it found, same doctrine as this platform's
-- other evidence tables.
CREATE OR REPLACE FUNCTION reject_control_run_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'subledger_control_runs is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_control_run_update
    BEFORE UPDATE ON subledger_control_runs
    FOR EACH ROW EXECUTE FUNCTION reject_control_run_mutation();
CREATE TRIGGER trg_reject_control_run_delete
    BEFORE DELETE ON subledger_control_runs
    FOR EACH ROW EXECUTE FUNCTION reject_control_run_mutation();

ALTER TABLE subledger_control_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE subledger_control_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subledger_control_runs
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_control_runs_entity_period ON subledger_control_runs (tenant_id, legal_entity_id, fiscal_period, subledger);
