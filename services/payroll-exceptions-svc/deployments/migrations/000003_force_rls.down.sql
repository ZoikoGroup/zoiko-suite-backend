-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE payroll_exceptions NO FORCE ROW LEVEL SECURITY;