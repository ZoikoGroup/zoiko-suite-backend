DROP TRIGGER IF EXISTS trg_accounting_periods_guard_update ON accounting_periods;
DROP TRIGGER IF EXISTS trg_accounting_periods_no_truncate ON accounting_periods;
DROP TRIGGER IF EXISTS trg_accounting_periods_no_delete ON accounting_periods;
DROP TRIGGER IF EXISTS trg_period_state_history_no_truncate ON period_state_history;
DROP TRIGGER IF EXISTS trg_period_state_history_append_only ON period_state_history;
DROP FUNCTION IF EXISTS accounting_period_guard_update();
DROP FUNCTION IF EXISTS accounting_period_forbid_delete();
DROP FUNCTION IF EXISTS accounting_period_forbid_mutation();
