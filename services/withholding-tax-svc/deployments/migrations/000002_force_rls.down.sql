-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE withholding_tax_obligations NO FORCE ROW LEVEL SECURITY;