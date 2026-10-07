-- Down migration for 000005_authority_limit.up.sql

ALTER TABLE delegation_grants
    DROP CONSTRAINT IF EXISTS delegation_grants_limit_currency_together;

ALTER TABLE delegation_grants
    DROP COLUMN IF EXISTS authority_limit_quantity,
    DROP COLUMN IF EXISTS authority_limit_currency,
    DROP COLUMN IF EXISTS authority_limit_cents;