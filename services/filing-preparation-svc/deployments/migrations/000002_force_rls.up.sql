-- Priority 3: Add FORCE ROW LEVEL SECURITY to all tenant-scoped tables.
-- RLS was already ENABLED with policies from migration 000001.
-- FORCE ensures the policy also applies to the table owner role.
-- See docs/architecture/backend-completion-tracker.md Priority 3.

ALTER TABLE filing_drafts FORCE ROW LEVEL SECURITY;

