-- Migration: 000005_pair_counterparty_period_dimensions.down.sql
--
-- Manual rollback only. Loses every pair's counterparty side, period and
-- dimensions; a cross-currency pair's counterparty amount cannot be
-- recovered from what remains.

DROP INDEX IF EXISTS idx_intercompany_tenant_period_entities;
ALTER TABLE intercompany_entries
    DROP CONSTRAINT IF EXISTS chk_dimensions_object,
    DROP CONSTRAINT IF EXISTS chk_fiscal_period_shape,
    DROP CONSTRAINT IF EXISTS chk_counterparty_amount_positive,
    DROP COLUMN IF EXISTS dimensions,
    DROP COLUMN IF EXISTS fiscal_period,
    DROP COLUMN IF EXISTS counterparty_currency_code,
    DROP COLUMN IF EXISTS counterparty_amount;
