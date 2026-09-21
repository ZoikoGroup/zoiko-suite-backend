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

ALTER TABLE treasury_transfers
    DROP COLUMN IF EXISTS authorizer_principal_id;
