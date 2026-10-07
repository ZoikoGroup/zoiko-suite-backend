DROP TABLE IF EXISTS ledger_balances;
DROP TRIGGER IF EXISTS trg_ledger_entries_no_delete ON ledger_entries;
DROP TRIGGER IF EXISTS trg_ledger_entries_no_update ON ledger_entries;
DROP FUNCTION IF EXISTS reject_ledger_entry_mutation();
DROP TABLE IF EXISTS ledger_entries;
