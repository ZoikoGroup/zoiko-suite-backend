-- Migration: 000007_add_consolidation_eliminations.up.sql
--
-- SPEC_DEVIATIONS A2/B9. A run's automatic elimination used to adjust the
-- in-memory group balances and record nothing but arithmetic, so "what did
-- run R eliminate, and why" had no answer (ExplainElimination answered from
-- the pair's current reciprocal balance instead, B8). This table is that
-- record: one row per (run, intercompany pair) for every MATCHED or CLOSED
-- pair in the run's scope, written once by the run and never changed.
--
--   outcome ELIMINATED: `lines` holds every journal line subtracted from the
--     group balances (side, journal, line number, account, debit, credit).
--   outcome BLOCKED: nothing of the pair was subtracted; blocked_reason says
--     why. A pair is never half eliminated.
--
-- intercompany_account_codes is the account set the run declared, the basis
-- on which lines were identified as intercompany. period_basis records how
-- the pair was put in the run's period (its own fiscal_period, or its source
-- journal's for a pair created before intercompany-accounting-svc's 000005).

CREATE TABLE consolidation_eliminations (
    elimination_id              UUID PRIMARY KEY,
    tenant_id                   VARCHAR(255) NOT NULL,
    consolidation_run_id        UUID NOT NULL REFERENCES consolidation_runs(consolidation_run_id),
    intercompany_entry_id       VARCHAR(255) NOT NULL,
    source_legal_entity_id      VARCHAR(255) NOT NULL,
    target_legal_entity_id      VARCHAR(255) NOT NULL,
    fiscal_period               VARCHAR(50)  NOT NULL,
    period_basis                VARCHAR(20)  NOT NULL,
    match_status                VARCHAR(30)  NOT NULL,
    source_journal_id           VARCHAR(255) NOT NULL,
    target_journal_id           VARCHAR(255),
    amount                      NUMERIC(18,4) NOT NULL,
    currency_code               VARCHAR(10)  NOT NULL DEFAULT '',
    counterparty_amount         NUMERIC(18,4),
    counterparty_currency_code  VARCHAR(10)  NOT NULL DEFAULT '',
    intercompany_account_codes  JSONB NOT NULL,
    outcome                     VARCHAR(20)  NOT NULL,
    blocked_reason              TEXT,
    lines                       JSONB NOT NULL,
    created_at                  TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT uq_consolidation_elimination_run_pair UNIQUE (consolidation_run_id, intercompany_entry_id),
    CONSTRAINT chk_consolidation_elimination_period_basis CHECK (period_basis IN ('PAIR', 'SOURCE_JOURNAL', 'UNDETERMINED')),
    CONSTRAINT chk_consolidation_elimination_match_status CHECK (match_status IN ('MATCHED', 'CLOSED')),
    CONSTRAINT chk_consolidation_elimination_accounts CHECK (jsonb_typeof(intercompany_account_codes) = 'array'),
    CONSTRAINT chk_consolidation_elimination_lines CHECK (jsonb_typeof(lines) = 'array'),
    -- ELIMINATED: at least one line, no reason, and a determined period.
    -- BLOCKED: no line and a reason.
    CONSTRAINT chk_consolidation_elimination_outcome CHECK (
        (outcome = 'ELIMINATED' AND blocked_reason IS NULL AND jsonb_array_length(lines) > 0
            AND period_basis <> 'UNDETERMINED' AND target_journal_id IS NOT NULL)
        OR (outcome = 'BLOCKED' AND blocked_reason IS NOT NULL AND length(blocked_reason) > 0
            AND jsonb_array_length(lines) = 0))
);

-- Append-only: the row is what the run did; correcting it would rewrite
-- history. A later run records its own rows.
CREATE OR REPLACE FUNCTION reject_consolidation_elimination_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'consolidation_eliminations is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_consolidation_elimination_update
    BEFORE UPDATE ON consolidation_eliminations
    FOR EACH ROW EXECUTE FUNCTION reject_consolidation_elimination_mutation();

CREATE TRIGGER trg_reject_consolidation_elimination_delete
    BEFORE DELETE ON consolidation_eliminations
    FOR EACH ROW EXECUTE FUNCTION reject_consolidation_elimination_mutation();

ALTER TABLE consolidation_eliminations ENABLE ROW LEVEL SECURITY;
ALTER TABLE consolidation_eliminations FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON consolidation_eliminations
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_consolidation_eliminations_run
    ON consolidation_eliminations (tenant_id, consolidation_run_id);
CREATE INDEX idx_consolidation_eliminations_pair
    ON consolidation_eliminations (tenant_id, intercompany_entry_id);
