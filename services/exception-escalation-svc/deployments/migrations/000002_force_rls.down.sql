-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE exception_cases NO FORCE ROW LEVEL SECURITY;
ALTER TABLE escalation_records NO FORCE ROW LEVEL SECURITY;