-- Migration: 000008_add_fx_revaluation.up.sql
--
-- ACC-10 (Foreign Currency Revaluation): "owns FX revaluation runs/item
-- calculations. Must never own: FX reference master or ledger write
-- bypass." Fuller ownership: "RevaluationRun, item calculations, rate
-- references, resulting posting refs."
--
-- "Must never own: FX reference master" means this capability is never
-- the platform's source of truth for current market rates — no
-- shared/global rate table exists here. Every run's closing_rate per
-- currency is caller-declared and recorded as THAT RUN's own permanent
-- evidence (fx_revaluation_items.closing_rate), never a row in a
-- cross-run master other services could come to depend on as
-- authoritative. Real book balances, by contrast, are never
-- caller-declared: they are read from general-ledger-svc's own trial
-- balance at run time (ACC-15), same doctrine as ACC-09's source amount.
--
-- fx_revaluation_runs is a normal mutable stateful row: REVIEW ->
-- APPROVED -> POSTED (see domain package doc comment for why PLANNED and
-- CALCULATED collapse into REVIEW in this v1 — StartRevaluation computes
-- synchronously, so there is no durable intermediate state to expose).
-- "Closing rate amended after approval" (negative path #3) is satisfied
-- structurally: there is no rate-edit endpoint at any stage, so a run's
-- rates can never be mutated once created, approved or not.
--
-- fx_revaluation_items is append-only calculation evidence — one row per
-- monetary item a run revalued, permanent from the moment the run is
-- created, regardless of whether the run is ever approved or posted.

CREATE TABLE fx_revaluation_runs (
    run_id                     UUID PRIMARY KEY,
    tenant_id                   VARCHAR(255) NOT NULL,
    legal_entity_id              VARCHAR(255) NOT NULL,
    fiscal_period                 VARCHAR(20) NOT NULL,
    fx_gain_loss_account_code      VARCHAR(64) NOT NULL,
    status                         VARCHAR(20) NOT NULL, -- REVIEW|APPROVED|POSTED
    reversal_of_run_id              UUID REFERENCES fx_revaluation_runs(run_id),
    journal_id                      VARCHAR(255),
    created_at                      TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id         VARCHAR(255) NOT NULL,
    approved_at                     TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id        VARCHAR(255),
    posted_at                       TIMESTAMP WITH TIME ZONE,
    posted_by_principal_id          VARCHAR(255)
);

CREATE TABLE fx_revaluation_items (
    item_id                UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    run_id                     UUID NOT NULL REFERENCES fx_revaluation_runs(run_id),
    account_code                VARCHAR(64) NOT NULL,
    account_type                 VARCHAR(20) NOT NULL, -- ASSET | LIABILITY, snapshotted at calculation time
    currency_code                 VARCHAR(3) NOT NULL,
    foreign_amount                 NUMERIC(18,2) NOT NULL, -- the item's face value in its foreign currency
    book_amount                     NUMERIC(18,2) NOT NULL, -- functional-currency amount currently on GL's books (read from trial balance)
    closing_rate                    NUMERIC(18,6) NOT NULL,
    revalued_amount                  NUMERIC(18,2) NOT NULL, -- foreign_amount * closing_rate
    adjustment_amount                 NUMERIC(18,2) NOT NULL  -- revalued_amount - book_amount
);

CREATE OR REPLACE FUNCTION reject_fx_item_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'fx_revaluation_items is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_fx_item_update
    BEFORE UPDATE ON fx_revaluation_items
    FOR EACH ROW EXECUTE FUNCTION reject_fx_item_mutation();
CREATE TRIGGER trg_reject_fx_item_delete
    BEFORE DELETE ON fx_revaluation_items
    FOR EACH ROW EXECUTE FUNCTION reject_fx_item_mutation();

ALTER TABLE fx_revaluation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_revaluation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_revaluation_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE fx_revaluation_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_revaluation_items FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_revaluation_items
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_fx_revaluation_runs_entity_period ON fx_revaluation_runs (tenant_id, legal_entity_id, fiscal_period);
CREATE INDEX idx_fx_revaluation_items_run ON fx_revaluation_items (tenant_id, run_id);
