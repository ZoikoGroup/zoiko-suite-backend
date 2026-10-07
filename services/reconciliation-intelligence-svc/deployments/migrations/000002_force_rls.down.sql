-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE reconciliation_jobs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_unmatched_items NO FORCE ROW LEVEL SECURITY;