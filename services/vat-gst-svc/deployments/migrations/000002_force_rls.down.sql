-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE vat_returns NO FORCE ROW LEVEL SECURITY;