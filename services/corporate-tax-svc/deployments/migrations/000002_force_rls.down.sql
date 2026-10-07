-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE corporate_tax_returns NO FORCE ROW LEVEL SECURITY;