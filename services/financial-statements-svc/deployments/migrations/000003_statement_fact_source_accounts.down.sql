ALTER TABLE statement_facts DROP CONSTRAINT IF EXISTS statement_facts_source_accounts_is_array;
ALTER TABLE statement_facts DROP COLUMN IF EXISTS source_accounts;
