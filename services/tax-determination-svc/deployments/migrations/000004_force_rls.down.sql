-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE tax_determinations NO FORCE ROW LEVEL SECURITY;