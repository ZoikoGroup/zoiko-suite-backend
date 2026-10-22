-- Migration: 000005_pair_counterparty_period_dimensions.up.sql
--
-- SPEC_DEVIATIONS A4. ACC-11's required inputs are "originating entity;
-- counterparty entity; source transaction; currencies; reciprocal refs;
-- dimensions" (§14), and the §24 IntercompanyPair carries "entity A/B refs,
-- currencies, reciprocal IDs". Until now a pair had one amount and one
-- currency, no period and no dimensions, and MatchIntercompany compared the
-- target journal to that single amount without ever looking at currency.
--
-- amount / currency_code stay the originating (source) side.
-- counterparty_amount / counterparty_currency_code are what the target
-- entity is expected to book; the match now compares the target journal
-- against these, currency first.
--
-- Backfill: every existing pair is given counterparty = source. That is not
-- an assumption introduced here; it is exactly what the old match did (it
-- compared the target journal to `amount`), so existing pairs keep the
-- behaviour they were created under.
--
-- fiscal_period is left NULL on existing rows. The true period is the source
-- journal's, and inventing one from created_at would put pairs in the wrong
-- period near a boundary. A NULL period means "not recorded".

ALTER TABLE intercompany_entries
    ADD COLUMN counterparty_amount        NUMERIC(18,4),
    ADD COLUMN counterparty_currency_code VARCHAR(10),
    ADD COLUMN fiscal_period              VARCHAR(20),
    ADD COLUMN dimensions                 JSONB NOT NULL DEFAULT '{}'::jsonb;

UPDATE intercompany_entries
SET counterparty_amount = amount,
    counterparty_currency_code = currency_code;

ALTER TABLE intercompany_entries
    ALTER COLUMN counterparty_amount SET NOT NULL,
    ALTER COLUMN counterparty_currency_code SET NOT NULL,
    ADD CONSTRAINT chk_counterparty_amount_positive CHECK (counterparty_amount > 0),
    ADD CONSTRAINT chk_fiscal_period_shape CHECK (fiscal_period IS NULL OR length(fiscal_period) BETWEEN 1 AND 20),
    ADD CONSTRAINT chk_dimensions_object CHECK (jsonb_typeof(dimensions) = 'object');

-- Serves period-scoped reads by entity pair (GetReciprocalBalances, A1, and
-- consolidation's period scoping), which this column exists to unblock.
CREATE INDEX idx_intercompany_tenant_period_entities
    ON intercompany_entries (tenant_id, fiscal_period, source_legal_entity_id, target_legal_entity_id);
