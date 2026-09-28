-- BNK-05: Switch match source from GL journals to banking-connector-svc
-- canonical transactions. Adds matched_transaction_id alongside the existing
-- matched_journal_id (kept for backward compatibility during migration).
-- Also adds proposed_transaction_id for the maker-checker path.
ALTER TABLE statement_lines
    ADD COLUMN matched_transaction_id  VARCHAR(64),
    ADD COLUMN proposed_transaction_id VARCHAR(64);

COMMENT ON COLUMN statement_lines.matched_transaction_id IS
    'banking-connector-svc canonical transaction ID; preferred over matched_journal_id';
COMMENT ON COLUMN statement_lines.proposed_transaction_id IS
    'banking-connector-svc canonical transaction ID proposed in maker-checker path';
