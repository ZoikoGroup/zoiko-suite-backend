-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE intercompany_entries NO FORCE ROW LEVEL SECURITY;