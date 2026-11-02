-- Reverts 000040. QUANTITY limits cannot be represented without limit_kind,
-- so they are removed first: left behind, each would read as a monetary limit
-- in GBP.
DELETE FROM authority_limits WHERE limit_kind = 'QUANTITY';

ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_quantity_whole_check;
ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_kind_currency_check;
ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_limit_kind_check;
UPDATE authority_limits SET currency = 'GBP' WHERE currency IS NULL;
ALTER TABLE authority_limits ALTER COLUMN currency SET NOT NULL;
ALTER TABLE authority_limits DROP COLUMN IF EXISTS limit_kind;
