-- BNK-03 reprocess path. Migration 004 made QUARANTINED terminal for every
-- update, which also blocked the documented ReprocessQuarantinedStatement
-- command (QUARANTINED -> RECEIVED): every reprocess call failed at the
-- trigger before the application could see a row.
--
-- The exception is deliberately narrow. A quarantined header may only move
-- back to RECEIVED with an empty quarantine_reason; every other column must
-- be byte-for-byte unchanged, so reprocessing cannot be used to rewrite
-- immutable statement evidence. ACCEPTED and REJECTED remain terminal.
CREATE OR REPLACE FUNCTION reject_terminal_statement_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_statements rows are never deleted';
    END IF;
    IF OLD.status = 'QUARANTINED' AND NEW.status = 'RECEIVED' THEN
        IF NEW.quarantine_reason <> '' THEN
            RAISE EXCEPTION 'reprocessing bank statement % must clear its quarantine reason', OLD.statement_id;
        END IF;
        IF ROW(
            NEW.statement_id, NEW.connection_id, NEW.tenant_id, NEW.statement_format,
            NEW.statement_date, NEW.opening_balance, NEW.closing_balance,
            NEW.transaction_count, NEW.ingested_at, NEW.content_hash, NEW.source_id,
            NEW.import_batch_id, NEW.created_by_principal_id
        ) IS DISTINCT FROM ROW(
            OLD.statement_id, OLD.connection_id, OLD.tenant_id, OLD.statement_format,
            OLD.statement_date, OLD.opening_balance, OLD.closing_balance,
            OLD.transaction_count, OLD.ingested_at, OLD.content_hash, OLD.source_id,
            OLD.import_batch_id, OLD.created_by_principal_id
        ) THEN
            RAISE EXCEPTION 'reprocessing bank statement % may only change status and quarantine_reason', OLD.statement_id;
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status NOT IN ('RECEIVED', 'VALIDATING') THEN
        RAISE EXCEPTION 'bank statement % is % and can no longer be modified', OLD.statement_id, OLD.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
