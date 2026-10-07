-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE filing_requirements NO FORCE ROW LEVEL SECURITY;