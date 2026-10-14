-- Revert FORCE ROW LEVEL SECURITY (leave ENABLE and policies intact).

ALTER TABLE migration_jobs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE migration_integrity_checks NO FORCE ROW LEVEL SECURITY;
ALTER TABLE migration_audit_entries NO FORCE ROW LEVEL SECURITY;