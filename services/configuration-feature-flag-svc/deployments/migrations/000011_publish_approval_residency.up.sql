-- 000011_publish_approval_residency
--
-- Two declaration-level controls AA-001 names and 000005 left out.
--
-- 1. §10.1 "Publish immutable version — approval binding for S2/S3". A
--    published version of an authority-bearing key now records the approval
--    that allowed it; the store refuses an S2/S3 publish without one. The
--    reference names the approval record (WFC/change ticket); the approving
--    principal is recorded beside it for segregation-of-duties review.
--
-- 2. INV-26 "Environment, region and residency constraints are evaluated
--    before secret attachment or runtime delivery". A key may declare the
--    jurisdictions it may be delivered to; NULL means unrestricted. Resolution
--    refuses delivery to a caller whose gateway-verified jurisdiction is not
--    listed.

ALTER TABLE config_definition_versions
    ADD COLUMN IF NOT EXISTS approval_reference TEXT,
    ADD COLUMN IF NOT EXISTS approved_by_principal_id TEXT;

ALTER TABLE config_definitions
    ADD COLUMN IF NOT EXISTS allowed_regions JSONB;
