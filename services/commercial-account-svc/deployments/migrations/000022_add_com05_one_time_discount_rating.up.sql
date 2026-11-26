-- 000022_add_com05_one_time_discount_rating.up.sql
-- COM-05 Platform Commercial Billing, part 5a follow-up (ZS-SVC-Q-001 §4.5,
-- §5). Closes the rating gap migration 000012 explicitly left open: ONE_TIME
-- and DISCOUNT price components are valid, can be bound to a subscription
-- (migration 000006/000009), but RateInvoiceBasis never rated them onto an
-- invoice. §5's rating doctrine lists "discount policy versions" as a
-- required rating input, and COM-05's own "Required inputs" row names
-- "authorized discounts/credits" -- this was a silent omission, not a
-- documented scope boundary.
--
-- Two new line kinds:
--   ONE_TIME  -- a component's flat amount, billed at most once ever for a
--               given (price_version_id, component_key) on one subscription.
--               "Already billed" is derived from this subscription's own
--               immutable, issued invoice_lines (COM-CTRL-022): never a
--               separate mutable flag that could drift from the evidence.
--   DISCOUNT  -- a negative-amount line reducing the invoice's subtotal,
--               applied only for an APPROVED subscription_discounts
--               application (migration 000009; COM-CTRL-005 delegated
--               authority -- an unapproved discount is never applied), for
--               that application's duration_intervals counted from the term
--               it was approved in.
--
-- discount_application_id is new lineage on a DISCOUNT line (COM-CTRL-019):
-- which approved application authorized this exact reduction, sealed at
-- generate time same as every other line's basis.

ALTER TABLE invoice_candidate_lines DROP CONSTRAINT invoice_candidate_lines_kind_check;
ALTER TABLE invoice_candidate_lines ADD CONSTRAINT invoice_candidate_lines_kind_check
    CHECK (kind IN ('RECURRING', 'USAGE', 'ONE_TIME', 'DISCOUNT'));

ALTER TABLE invoice_candidate_lines ADD COLUMN discount_application_id TEXT
    REFERENCES subscription_discounts (discount_application_id);
ALTER TABLE invoice_candidate_lines ADD CONSTRAINT invoice_candidate_lines_discount_has_application CHECK (
    (kind = 'DISCOUNT') = (discount_application_id IS NOT NULL));

ALTER TABLE invoice_lines DROP CONSTRAINT invoice_lines_kind_check;
ALTER TABLE invoice_lines ADD CONSTRAINT invoice_lines_kind_check
    CHECK (kind IN ('RECURRING', 'USAGE', 'ONE_TIME', 'DISCOUNT'));

ALTER TABLE invoice_lines ADD COLUMN discount_application_id TEXT
    REFERENCES subscription_discounts (discount_application_id);
ALTER TABLE invoice_lines ADD CONSTRAINT invoice_lines_discount_has_application CHECK (
    (kind = 'DISCOUNT') = (discount_application_id IS NOT NULL));

-- Fast "has this ONE_TIME component already been billed on an issued
-- invoice for this subscription" lookup (GenerateInvoiceCandidate runs this
-- on every candidate generation).
CREATE INDEX idx_invoice_lines_subscription_one_time ON invoice_lines (price_version_id, component_key)
    WHERE kind = 'ONE_TIME';
