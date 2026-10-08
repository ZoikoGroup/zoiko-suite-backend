-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE employment_contracts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE contract_amendments NO FORCE ROW LEVEL SECURITY;