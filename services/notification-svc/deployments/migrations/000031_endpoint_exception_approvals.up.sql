-- Migration 000031: NP-11's controlled endpoint exception is a recorded,
-- second-principal approval (Group 1 re-audit gap S7-1 / R-6).
--
-- exception_ref, verification_ref and reviewer_principal_id were request
-- strings, none looked up, and the free-text endpoint was then marked
-- verified. An exception is now an ncd_approvals row of kind
-- ENDPOINT_EXCEPTION (POST /v1/recipient-endpoint-exceptions), decided by a
-- principal other than its requester (the table's ncd_approval_sod CHECK),
-- and a resolution honours exception_ref only when it is APPROVED for the same
-- recipient, address and verification, by the named reviewer.

ALTER TABLE ncd_approvals DROP CONSTRAINT IF EXISTS ncd_approvals_kind_check;
ALTER TABLE ncd_approvals ADD CONSTRAINT ncd_approvals_kind_check
    CHECK (kind IN ('RESEND', 'MANUAL_EVIDENCE', 'SUPPRESSION_LIFT', 'BULK_SEND', 'ENDPOINT_EXCEPTION'));
