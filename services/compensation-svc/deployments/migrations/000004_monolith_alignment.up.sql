-- Monolith alignment: the compensation API gained structure metadata, wage
-- revision audit fields and bonus payout detail (services/compensation-svc
-- internal/store/pg_store.go), but the columns were added to the Go code
-- without a migration, so every INSERT/SELECT against a schema built from
-- these files failed with 42703 (column does not exist). This brings the
-- schema up to what the store already reads and writes.

-- Structure catalog: what the band is made of and where it applies.
ALTER TABLE compensation_structures
    ADD COLUMN description         TEXT,
    ADD COLUMN grade_code          VARCHAR(100),
    ADD COLUMN level_code          VARCHAR(100),
    -- NOT NULL: the Go type is a plain bool (not *bool), so a NULL would
    -- fail every Scan of ListStructures/CreateStructure.
    ADD COLUMN is_default          BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN applicable_location VARCHAR(100);

-- Wage revisions: an audit trail of what the amount was before and who
-- approved the change. previous_amount/previous_currency are nullable:
-- the first revision for an employee has no predecessor.
ALTER TABLE wage_revisions
    ADD COLUMN previous_amount   NUMERIC(18, 4),
    ADD COLUMN previous_currency VARCHAR(3),
    -- NOT NULL DEFAULT '': the Go type is a plain string, so NULL would
    -- fail Scan; an unset revision type is the empty string, not a gap.
    ADD COLUMN revision_type     VARCHAR(50) NOT NULL DEFAULT '',
    ADD COLUMN approved_by       VARCHAR(255),
    ADD COLUMN approved_at       TIMESTAMP WITH TIME ZONE;

-- Bonus grants: payout detail, so an approved grant can be reconciled
-- against the payment that settled it.
ALTER TABLE bonus_grants
    ADD COLUMN paid_at           TIMESTAMP WITH TIME ZONE,
    ADD COLUMN payment_reference VARCHAR(255),
    ADD COLUMN payout_period     VARCHAR(50),
    ADD COLUMN taxable_amount    NUMERIC(18, 4),
    ADD COLUMN conditions        TEXT,
    ADD COLUMN notes             TEXT;
