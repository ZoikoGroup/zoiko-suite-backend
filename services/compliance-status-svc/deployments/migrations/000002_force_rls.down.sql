-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE compliance_status_records NO FORCE ROW LEVEL SECURITY;
ALTER TABLE compliance_gaps NO FORCE ROW LEVEL SECURITY;