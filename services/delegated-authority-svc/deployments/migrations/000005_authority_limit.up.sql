-- AuthorityLimit: add monetary/quantitative ceiling to delegation grants.
-- ORG-06 §4.5: "A delegation carries an action but no monetary or quantitative ceiling."

ALTER TABLE delegation_grants
    ADD COLUMN IF NOT EXISTS authority_limit_cents BIGINT,      -- e.g., 50000 = 500.00
    ADD COLUMN IF NOT EXISTS authority_limit_currency VARCHAR(3); -- ISO 4217, e.g., USD

-- For non-monetary limits (e.g., "max 10 invoices"), use a separate column.
-- The monetary limit is the primary use case per the audit.
ALTER TABLE delegation_grants
    ADD COLUMN IF NOT EXISTS authority_limit_quantity BIGINT;    -- e.g., max count

-- Constraint: if authority_limit_cents is set, currency must be set, and vice versa.
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_limit_currency_together
    CHECK ((authority_limit_cents IS NULL) = (authority_limit_currency IS NULL)) NOT VALID;