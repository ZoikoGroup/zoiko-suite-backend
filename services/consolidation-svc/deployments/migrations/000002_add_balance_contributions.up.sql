-- Migration: 000002_add_balance_contributions.up.sql
--
-- ACC-13 (Consolidation) gap: a group balance's own signed snapshot proves
-- the FINAL number wasn't tampered with, but nothing recorded which child
-- entities' balances actually summed into it — the group-balance math ran
-- entirely in memory (consolidatedBalances) and was discarded once the
-- snapshot was written. This table is that missing entity-to-group
-- provenance: one row per (run, account_code, source child entity),
-- recorded BEFORE elimination, so a later reader can answer "which
-- entities' balances make up this group line" for real, not by re-running
-- the consolidation and hoping nothing changed since.

CREATE TABLE IF NOT EXISTS balance_contributions (
    balance_contribution_id UUID PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    consolidation_run_id    UUID NOT NULL REFERENCES consolidation_runs(consolidation_run_id) ON DELETE CASCADE,
    account_code            VARCHAR(100) NOT NULL,
    source_legal_entity_id  VARCHAR(255) NOT NULL, -- the CHILD entity, never the group itself
    gross_amount            NUMERIC(18,4) NOT NULL, -- pre-elimination, exactly as that entity's own trial balance reported it
    generated_at            TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Append-only: a contribution row is a permanent record of what one child
-- entity reported into one group-level run. Never updated or deleted from
-- application code, and the trigger below makes that a database-enforced
-- invariant, same doctrine as this platform's other append-only evidence
-- tables.
CREATE OR REPLACE FUNCTION reject_balance_contribution_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'balance_contributions is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_balance_contribution_update
    BEFORE UPDATE ON balance_contributions
    FOR EACH ROW EXECUTE FUNCTION reject_balance_contribution_mutation();

CREATE TRIGGER trg_reject_balance_contribution_delete
    BEFORE DELETE ON balance_contributions
    FOR EACH ROW EXECUTE FUNCTION reject_balance_contribution_mutation();

ALTER TABLE balance_contributions ENABLE ROW LEVEL SECURITY;
ALTER TABLE balance_contributions FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON balance_contributions
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_balance_contributions_run ON balance_contributions (consolidation_run_id);
CREATE INDEX idx_balance_contributions_run_account ON balance_contributions (consolidation_run_id, account_code);
