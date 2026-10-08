-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE filing_drafts NO FORCE ROW LEVEL SECURITY;