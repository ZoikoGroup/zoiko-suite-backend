-- ZS-SVC-M-001 INT-09 (E-invoicing network adapter), on the same governed attempt machinery as INT-08.
--
--   * an interface says what it carries: TAX (default) or EINVOICE;
--   * an e-invoice has a state a tax filing does not: ACCEPTED (the network cleared it) is followed by DELIVERED
--     (the recipient has it), so ACCEPTED is final for a tax filing but may move to DELIVERED for an e-invoice;
--   * validation PINS the profile version: a profile that changes after approval never silently re-transforms an
--     approved document; the filing keeps the version it was validated under (the guard already forbids changing it).
-- Re-runnable. The guard function below starts from the LATEST version (003) and only adds the rules above.

ALTER TABLE tax_interfaces
    ADD COLUMN IF NOT EXISTS adapter_kind TEXT NOT NULL DEFAULT 'TAX';
ALTER TABLE tax_interfaces DROP CONSTRAINT IF EXISTS tax_interface_adapter_kind_check;
ALTER TABLE tax_interfaces ADD CONSTRAINT tax_interface_adapter_kind_check CHECK (adapter_kind IN ('TAX', 'EINVOICE'));

ALTER TABLE tax_filing_submissions DROP CONSTRAINT IF EXISTS tax_filing_status_check;
ALTER TABLE tax_filing_submissions ADD CONSTRAINT tax_filing_status_check CHECK (status IN
    ('PENDING', 'PREPARED', 'VALIDATED', 'BLOCKED', 'SUBMITTING', 'SUBMITTED', 'PENDING_UNKNOWN', 'ACCEPTED', 'DELIVERED', 'REJECTED', 'SUPERSEDED'));

DROP INDEX IF EXISTS idx_tax_filing_poll;
CREATE INDEX IF NOT EXISTS idx_tax_filing_poll ON tax_filing_submissions (next_poll_at) WHERE status IN ('SUBMITTED', 'PENDING_UNKNOWN', 'ACCEPTED');

CREATE OR REPLACE FUNCTION tax_filing_guard() RETURNS TRIGGER AS $$
DECLARE
    kind TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'tax filing submissions are never deleted';
    END IF;
    SELECT adapter_kind INTO kind FROM tax_interfaces WHERE interface_id = OLD.interface_id AND tenant_id = OLD.tenant_id;

    -- final states: REJECTED, SUPERSEDED, BLOCKED and DELIVERED always; ACCEPTED too, except that an e-invoice
    -- accepted by its network may still be delivered
    IF NEW.status IS DISTINCT FROM OLD.status AND (
        OLD.status IN ('REJECTED', 'SUPERSEDED', 'BLOCKED', 'DELIVERED')
        OR (OLD.status = 'ACCEPTED' AND NOT (kind = 'EINVOICE' AND NEW.status = 'DELIVERED'))) THEN
        RAISE EXCEPTION 'filing % is in terminal status % and cannot change', OLD.submission_id, OLD.status;
    END IF;
    IF OLD.status IN ('ACCEPTED', 'REJECTED', 'SUPERSEDED', 'BLOCKED', 'DELIVERED') AND OLD.authority_ref <> '' AND (
        NEW.authority_ref IS DISTINCT FROM OLD.authority_ref OR NEW.receipt_hash IS DISTINCT FROM OLD.receipt_hash)
       AND NOT (OLD.status = 'ACCEPTED' AND NEW.status = 'DELIVERED') THEN
        RAISE EXCEPTION 'filing % is final; its authority receipt cannot change', OLD.submission_id;
    END IF;

    -- what was approved can never change: a changed document is a different subject
    IF NEW.interface_id IS DISTINCT FROM OLD.interface_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.tax_period IS DISTINCT FROM OLD.tax_period OR NEW.filing_type IS DISTINCT FROM OLD.filing_type
        OR NEW.tax_amount IS DISTINCT FROM OLD.tax_amount
        OR (OLD.payload_hash <> '' AND (NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
              OR NEW.document IS DISTINCT FROM OLD.document
              OR NEW.subject_ref IS DISTINCT FROM OLD.subject_ref
              OR NEW.subject_fingerprint IS DISTINCT FROM OLD.subject_fingerprint
              OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at))
    THEN
        RAISE EXCEPTION 'filing % was approved as submitted; its subject can never change', OLD.submission_id;
    END IF;

    -- the profile, schema rail and transformed payload are pinned once chosen
    IF (OLD.rail <> '' AND NEW.rail IS DISTINCT FROM OLD.rail)
        OR (OLD.profile_version IS NOT NULL AND NEW.profile_version IS DISTINCT FROM OLD.profile_version)
        OR (OLD.sent_payload_hash <> '' AND NEW.sent_payload_hash IS DISTINCT FROM OLD.sent_payload_hash)
    THEN
        RAISE EXCEPTION 'filing % profile context is pinned; a retry is sent exactly as the original', OLD.submission_id;
    END IF;
    IF NEW.send_count < OLD.send_count THEN
        RAISE EXCEPTION 'filing % send_count can only increase', OLD.submission_id;
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NEW.status = 'SUBMITTING' AND OLD.status NOT IN ('PREPARED', 'VALIDATED', 'PENDING_UNKNOWN') THEN
            RAISE EXCEPTION 'filing % cannot start sending from %', OLD.submission_id, OLD.status;
        END IF;
        IF NEW.status IN ('ACCEPTED', 'REJECTED') AND OLD.status NOT IN ('SUBMITTING', 'SUBMITTED', 'PENDING_UNKNOWN') THEN
            RAISE EXCEPTION 'filing % cannot be % without having been sent', OLD.submission_id, NEW.status;
        END IF;
        IF NEW.status = 'DELIVERED' AND (kind IS DISTINCT FROM 'EINVOICE' OR OLD.status NOT IN ('SUBMITTING', 'SUBMITTED', 'PENDING_UNKNOWN', 'ACCEPTED')) THEN
            RAISE EXCEPTION 'filing % cannot be delivered from % (only a sent e-invoice can be)', OLD.submission_id, OLD.status;
        END IF;
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
