-- 000040: quantity (non-monetary) authority limits (decision S8-D1).
--
-- S8-D1 placed a principal's quantitative authority limit here, in
-- authority_limits, beside the monetary one. Every limit was an amount in a
-- currency, so "may approve at most 50 purchase-order lines" or "may release
-- at most 10 payments in one batch" had nowhere to live — and a delegator's
-- own quantity cap, which a delegation must not exceed (ORG-06 negative case
-- 11), could not be recorded at all.
--
--   limit_kind  MONETARY (every existing row) or QUANTITY.
--   currency    required for MONETARY, absent for QUANTITY: a count has no
--               currency, and a currency on one would invite conversion.
--   bounds      a QUANTITY limit counts whole things.
--
-- A QUANTITY limit is checked against the request's `quantity` attribute, the
-- attribute the delegation ceiling already reads.

ALTER TABLE authority_limits
    ADD COLUMN IF NOT EXISTS limit_kind VARCHAR(16) NOT NULL DEFAULT 'MONETARY';

ALTER TABLE authority_limits ALTER COLUMN currency DROP NOT NULL;

ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_limit_kind_check;
ALTER TABLE authority_limits ADD CONSTRAINT authority_limits_limit_kind_check
    CHECK (limit_kind IN ('MONETARY', 'QUANTITY'));

ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_kind_currency_check;
ALTER TABLE authority_limits ADD CONSTRAINT authority_limits_kind_currency_check
    CHECK ((limit_kind = 'MONETARY' AND currency IS NOT NULL)
        OR (limit_kind = 'QUANTITY' AND currency IS NULL));

ALTER TABLE authority_limits DROP CONSTRAINT IF EXISTS authority_limits_quantity_whole_check;
ALTER TABLE authority_limits ADD CONSTRAINT authority_limits_quantity_whole_check
    CHECK (limit_kind <> 'QUANTITY'
        OR (lower_limit = trunc(lower_limit) AND upper_limit = trunc(upper_limit)));
