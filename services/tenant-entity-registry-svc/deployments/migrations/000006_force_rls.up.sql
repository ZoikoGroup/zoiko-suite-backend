-- Priority 3: Add FORCE ROW LEVEL SECURITY to all tenant-scoped tables.
-- RLS was already ENABLED with policies from migration 000001.
-- FORCE ensures the policy also applies to the table owner role, providing
-- defence-in-depth if the table owner ever connects directly (e.g. manual psql).
-- Under the normal runtime role (zoiko_app, NOSUPERUSER NOBYPASSRLS) this has
-- no effect on live traffic -- it is insurance against a future regression.
-- See docs/architecture/backend-completion-tracker.md Priority 3.

ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
ALTER TABLE data_residency_policies FORCE ROW LEVEL SECURITY;
ALTER TABLE legal_entities FORCE ROW LEVEL SECURITY;
ALTER TABLE entity_hierarchies FORCE ROW LEVEL SECURITY;
ALTER TABLE entity_jurisdiction_assignments FORCE ROW LEVEL SECURITY;
ALTER TABLE tax_identity_bundles FORCE ROW LEVEL SECURITY;
ALTER TABLE workspaces FORCE ROW LEVEL SECURITY;