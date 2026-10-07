-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE tax_rules NO FORCE ROW LEVEL SECURITY;