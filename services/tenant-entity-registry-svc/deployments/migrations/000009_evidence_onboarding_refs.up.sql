-- 000009_evidence_onboarding_refs.up.sql
--
-- Closes the ORG-02 evidence gap from the 23 Sep 2026 Group 1 audit: the
-- lifecycle-history lineage record carried actor, reason, approver, command
-- name and version but no reference to the onboarding request / external
-- customer key. The onboarding context now rides on every tenant_lifecycle_history
-- row that documents it, so `ListTenantLifecycleHistory` is self-contained
-- evidence instead of a join the reader would have to know to make.
--
-- Columns stay NULL for commands to which onboarding context does not apply
-- (a locale change, for example).

ALTER TABLE tenant_lifecycle_history
    ADD COLUMN onboarding_request_ref VARCHAR(255),
    ADD COLUMN external_customer_key  VARCHAR(255);