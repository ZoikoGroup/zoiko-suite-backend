-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE termination_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE offboarding_checklists NO FORCE ROW LEVEL SECURITY;
ALTER TABLE checklist_items NO FORCE ROW LEVEL SECURITY;