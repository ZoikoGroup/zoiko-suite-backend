-- Record who logged and who resolved each compliance gap.
--
-- The gap carried no actor at all, so resolve could not enforce segregation
-- of duties (the principal who logged a gap may not resolve it). Both columns
-- are filled from the gateway-verified X-Principal-Id. Rows created before
-- this migration keep NULL: their creator was never recorded.
ALTER TABLE compliance_gaps ADD COLUMN IF NOT EXISTS created_by  TEXT;
ALTER TABLE compliance_gaps ADD COLUMN IF NOT EXISTS resolved_by TEXT;
