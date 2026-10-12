-- Immutability, enforced in the database so that neither a bug in this service
-- nor a fix-up script can rewrite closed-period history.

CREATE OR REPLACE FUNCTION accounting_period_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% on % is forbidden: this table is append-only', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION accounting_period_forbid_delete() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% on % is forbidden: periods are never deleted; change their state through a command', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_period_state_history_append_only
    BEFORE UPDATE OR DELETE ON period_state_history
    FOR EACH ROW EXECUTE FUNCTION accounting_period_forbid_mutation();
-- TRUNCATE bypasses row triggers; forbid it as well.
CREATE TRIGGER trg_period_state_history_no_truncate
    BEFORE TRUNCATE ON period_state_history
    FOR EACH STATEMENT EXECUTE FUNCTION accounting_period_forbid_mutation();

CREATE TRIGGER trg_accounting_periods_no_delete
    BEFORE DELETE ON accounting_periods
    FOR EACH ROW EXECUTE FUNCTION accounting_period_forbid_delete();
CREATE TRIGGER trg_accounting_periods_no_truncate
    BEFORE TRUNCATE ON accounting_periods
    FOR EACH STATEMENT EXECUTE FUNCTION accounting_period_forbid_delete();

-- Boundaries are inherited from the fiscal-calendar version and never move once
-- materialised; identity and scope never change. Only state, version, the reopen
-- window and updated_at may change -- and the state only along the lifecycle
-- (OPEN -> SOFT_CLOSED -> HARD_CLOSED -> REOPEN_AUTHORIZED -> RECLOSED
--  -> REOPEN_AUTHORIZED ...), one step per version bump.
CREATE OR REPLACE FUNCTION accounting_period_guard_update() RETURNS trigger AS $$
BEGIN
    IF NEW.period_id <> OLD.period_id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.calendar_id <> OLD.calendar_id
       OR NEW.calendar_version_id <> OLD.calendar_version_id OR NEW.book_scope <> OLD.book_scope
       OR NEW.module_scope <> OLD.module_scope OR NEW.period_key <> OLD.period_key
       OR NEW.fiscal_year <> OLD.fiscal_year OR NEW.period_no <> OLD.period_no
       OR NEW.start_date <> OLD.start_date OR NEW.end_date <> OLD.end_date
       OR NEW.kind <> OLD.kind OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'period identity, scope and boundaries are immutable once materialised'
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.state <> OLD.state THEN
        IF NOT ((OLD.state = 'OPEN' AND NEW.state = 'SOFT_CLOSED')
             OR (OLD.state = 'SOFT_CLOSED' AND NEW.state = 'HARD_CLOSED')
             OR (OLD.state = 'HARD_CLOSED' AND NEW.state = 'REOPEN_AUTHORIZED')
             OR (OLD.state = 'REOPEN_AUTHORIZED' AND NEW.state = 'RECLOSED')
             OR (OLD.state = 'RECLOSED' AND NEW.state = 'REOPEN_AUTHORIZED')) THEN
            RAISE EXCEPTION 'illegal period state transition % -> %', OLD.state, NEW.state
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF NEW.version <> OLD.version + 1 THEN
            RAISE EXCEPTION 'a state change must bump version by exactly one' USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_accounting_periods_guard_update
    BEFORE UPDATE ON accounting_periods
    FOR EACH ROW EXECUTE FUNCTION accounting_period_guard_update();
