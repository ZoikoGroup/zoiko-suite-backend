-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE tax_jurisdiction_profiles NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tax_calculation_records NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tax_basis_audits NO FORCE ROW LEVEL SECURITY;