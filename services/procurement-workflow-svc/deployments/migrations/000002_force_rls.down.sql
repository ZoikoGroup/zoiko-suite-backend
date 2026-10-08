-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE procurement_cases NO FORCE ROW LEVEL SECURITY;