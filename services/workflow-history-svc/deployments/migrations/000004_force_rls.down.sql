-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE workflow_history_events NO FORCE ROW LEVEL SECURITY;