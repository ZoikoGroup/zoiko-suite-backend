-- BNK-09: the doc's own additional dual-control step for cross-entity
-- transfers ("Approved/Authorized" in the state model; SoD: "Maker
-- cannot authorize own transfer where policy applies"; "cross-entity
-- transfers may require dual-entity/controller approval"). Same-entity
-- transfers never reach AUTHORIZED — they go straight from APPROVED to
-- SUBMITTED, unchanged from before this migration.
ALTER TABLE treasury_transfers
    ADD COLUMN authorizer_principal_id TEXT NOT NULL DEFAULT '';

-- Mirrors the existing checker<>maker guard already in
-- reject_terminal_transfer_mutation (migration 000007) — same DB-level
-- enforcement posture, not just an application-layer check.
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
    IF NEW.authorizer_principal_id <> '' AND NEW.authorizer_principal_id = NEW.maker_principal_id THEN
        RAISE EXCEPTION 'treasury transfer % authorizer cannot be the same principal as the maker', OLD.transfer_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
