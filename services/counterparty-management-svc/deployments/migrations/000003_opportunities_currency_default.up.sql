BEGIN;

-- set default on new rows
ALTER TABLE opportunities
  ALTER COLUMN currency SET DEFAULT 'USD';

-- backfill any existing empty values to USD
UPDATE opportunities
SET currency = 'USD'
WHERE currency IS NULL OR currency = '';

COMMIT;
