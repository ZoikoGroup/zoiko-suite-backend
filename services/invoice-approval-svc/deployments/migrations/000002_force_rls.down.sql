-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE invoice_approval_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE approval_decisions NO FORCE ROW LEVEL SECURITY;