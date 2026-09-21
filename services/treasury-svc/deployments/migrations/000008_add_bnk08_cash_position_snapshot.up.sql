-- BNK-08 Wave 14: the spec'd CashPositionSnapshot entity — a real,
-- persisted Calculated -> Published -> Superseded lifecycle. Before this
-- migration, GetEffectiveCash was a pure live-composition read that never
-- persisted anything (same pattern BNK-10's FX exposure still
-- deliberately uses, see bnk10.go's own doc comment) — this is the
-- opposite: a real snapshot entity with its own history.
--
-- "Stale" is NOT a stored status value — the doc's own words: "stale is
-- derived from source freshness, not operator choice." A PUBLISHED
-- snapshot whose bank-balance component has aged past the staleness
-- threshold is reported as effectively stale at READ time
-- (GetCashPosition/GetCashPositionAsOf), never by mutating status here.
CREATE TABLE cash_position_snapshots (
    snapshot_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         UUID NOT NULL,
    reporting_currency      VARCHAR(3) NOT NULL,
    as_of_timestamp         TIMESTAMPTZ NOT NULL,

    bank_balance            NUMERIC(20, 4) NOT NULL,
    -- restricted_amount is a caller-supplied figure at calculate time —
    -- see domain.CalculateCashPositionParams's own doc comment: the
    -- CLASSIFICATION SOURCE (which accounts/amounts count as restricted)
    -- is a configuration decision not made by this migration or this
    -- wave; only the exclusion arithmetic is real here.
    restricted_amount       NUMERIC(20, 4) NOT NULL DEFAULT 0,
    pending_ap_commitments  NUMERIC(20, 4) NOT NULL DEFAULT 0,
    payroll_obligations     NUMERIC(20, 4) NOT NULL DEFAULT 0,
    tax_liabilities         NUMERIC(20, 4) NOT NULL DEFAULT 0,
    available_cash          NUMERIC(20, 4) NOT NULL,

    fx_rate_version         TEXT NOT NULL DEFAULT '',
    -- account_breakdown is the real data GetAccountDrilldown reads back —
    -- the per-account bank-balance lines that summed to bank_balance,
    -- captured at calculate time (same accounts CalculateCashPosition's
    -- composition loop already reads).
    account_breakdown       JSONB NOT NULL DEFAULT '[]',

    status                  VARCHAR(32) NOT NULL DEFAULT 'CALCULATED',
    has_stale_component     BOOLEAN NOT NULL DEFAULT FALSE,
    published_by_principal_id VARCHAR(128) NOT NULL DEFAULT '',
    published_at            TIMESTAMPTZ NULL,
    superseded_by           UUID REFERENCES cash_position_snapshots (snapshot_id),

    calculated_by_principal_id VARCHAR(128) NOT NULL,
    correlation_id           VARCHAR(255) NOT NULL DEFAULT '',
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_cash_position_snapshots_entity_currency
    ON cash_position_snapshots (tenant_id, legal_entity_id, reporting_currency, created_at DESC);

ALTER TABLE cash_position_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE cash_position_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON cash_position_snapshots
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- SUPERSEDED is fully terminal. Every calculated figure is immutable from
-- the moment the row is written — only status/published_by_principal_id/
-- published_at/superseded_by ever change afterward, and superseded_by may
-- only be set once (NULL -> a value).
CREATE OR REPLACE FUNCTION reject_cash_position_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'cash_position_snapshots rows are never deleted';
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'cash position snapshot % is SUPERSEDED and can no longer be modified', OLD.snapshot_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.reporting_currency IS DISTINCT FROM OLD.reporting_currency
        OR NEW.as_of_timestamp IS DISTINCT FROM OLD.as_of_timestamp
        OR NEW.bank_balance IS DISTINCT FROM OLD.bank_balance
        OR NEW.restricted_amount IS DISTINCT FROM OLD.restricted_amount
        OR NEW.pending_ap_commitments IS DISTINCT FROM OLD.pending_ap_commitments
        OR NEW.payroll_obligations IS DISTINCT FROM OLD.payroll_obligations
        OR NEW.tax_liabilities IS DISTINCT FROM OLD.tax_liabilities
        OR NEW.available_cash IS DISTINCT FROM OLD.available_cash
        OR NEW.fx_rate_version IS DISTINCT FROM OLD.fx_rate_version
        OR NEW.account_breakdown IS DISTINCT FROM OLD.account_breakdown
        OR NEW.has_stale_component IS DISTINCT FROM OLD.has_stale_component
        OR NEW.calculated_by_principal_id IS DISTINCT FROM OLD.calculated_by_principal_id
        OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'cash position snapshot % calculated figures are immutable once written', OLD.snapshot_id;
    END IF;
    IF OLD.superseded_by IS NOT NULL AND NEW.superseded_by IS DISTINCT FROM OLD.superseded_by THEN
        RAISE EXCEPTION 'cash position snapshot % is already superseded', OLD.snapshot_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_cash_position_mutation
    BEFORE UPDATE OR DELETE ON cash_position_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_cash_position_mutation();
