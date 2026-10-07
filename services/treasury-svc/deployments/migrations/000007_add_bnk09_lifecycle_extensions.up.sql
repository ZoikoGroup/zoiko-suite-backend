-- BNK-09 Wave 12: Draft/Returned/Cancelled states plus
-- AmendTreasuryTransfer/CancelBeforeSubmission/ResolveTreasuryTransfer —
-- the doc's state model is "Draft -> PendingApproval -> Approved/Authorized
-- -> Submitted/Pending -> Settled / Rejected / Returned / Cancelled";
-- migration 000004 only ever reached PENDING_APPROVAL as its first state
-- and had no Cancelled/Returned leg at all.
--
-- CreateTreasuryTransfer's default (no flag) is UNCHANGED — it still
-- creates PENDING_APPROVAL directly, same backward-compatibility posture
-- migration 000003 already established for BNK-01's RegisterBankAccount.
-- DRAFT is only reached when a caller opts in.
ALTER TABLE treasury_transfers
    ADD COLUMN cancel_reason    TEXT NOT NULL DEFAULT '',
    ADD COLUMN return_reason    TEXT NOT NULL DEFAULT '',
    ADD COLUMN resolution_note  TEXT NOT NULL DEFAULT '';

-- CANCELLED joins COMPLETED/REJECTED as terminal. RETURNED is deliberately
-- NOT terminal — ResolveTreasuryTransfer must be able to move it onward
-- to PENDING_APPROVAL (resubmit) or CANCELLED (abandon).
CREATE OR REPLACE FUNCTION reject_terminal_transfer_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'treasury_transfers rows are never deleted';
    END IF;
    IF OLD.status IN ('COMPLETED', 'REJECTED', 'CANCELLED') THEN
        RAISE EXCEPTION 'treasury transfer % is % and can no longer be modified', OLD.transfer_id, OLD.status;
    END IF;
    IF NEW.checker_principal_id <> '' AND NEW.checker_principal_id = NEW.maker_principal_id THEN
        RAISE EXCEPTION 'treasury transfer % checker cannot be the same principal as the maker', OLD.transfer_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
