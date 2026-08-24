-- 000003_add_rls.up.sql
-- Row-level security for evidence-manifest-svc (tracker row 14).
--
-- Two tables, two shapes:
--
--   evidence_manifests — carries tenant_id. Plain equality policy.
--
--   manifest_records   — carries NO tenant column. It reaches the tenant
--                        through manifest_id, so its policy resolves via a
--                        subquery on evidence_manifests.
--
-- ── Why this is the most consequential policy in the tier ─────────────
--
-- manifest_records.record_snapshot is a verbatim JSON copy of each source
-- record as it existed at generation time — governance decisions, access
-- decisions, workflow instances — deliberately snapshotted in full so a
-- manifest stays reconstructable when the source service is unavailable
-- (000001_initial_schema.up.sql). Doc 03 §14.4 makes these bundles the
-- artefact handed to an auditor, regulator or legal-discovery request.
--
-- So an unscoped read here is not a metadata leak. It is one tenant's
-- assembled evidence, in full, to whoever holds a manifest id. Before this
-- migration the service read no X-Tenant-Id at all and both GET routes
-- took only a manifest id from the URL.
--
-- ── The parent-coupling caveat, verified not assumed ──────────────────
--
-- manifest_records' policy reads evidence_manifests, and a policy's
-- subquery is itself subject to RLS on the table it reads. The two ways of
-- "removing" the parent policy therefore behave in OPPOSITE directions.
-- This was established empirically on Postgres 16 in commercial-account-svc
-- (tracker row 11c), where the intuitive guess turned out to be wrong:
--
--   DROP POLICY on evidence_manifests   → fails CLOSED.
--     RLS stays enabled with no applicable policy, which Postgres reads as
--     deny-all, so the subquery returns the empty set and manifest_records
--     becomes MORE restrictive — even the owning tenant loses its records.
--     An outage.
--
--   DISABLE ROW LEVEL SECURITY on it    → fails OPEN.
--     The subquery sees every manifest, and manifest_records widens to
--     every tenant's evidence at once.
--
-- One ALTER TABLE is a breach; dropping the same table's policy is only an
-- outage; they look equally innocuous in a diff. Asserted by
-- TestRLS_ParentPolicyCoupling in internal/store/rls_test.go.
--
-- ── Type handling ────────────────────────────────────────────────────
--
-- tenant_id is UUID, compared as ::text against the GUC rather than casting
-- the GUC to uuid: app.tenant_id can legitimately be the empty string
-- (TenantFromContext returns "" when no verified tenant is present, by
-- design), and ''::uuid raises invalid input syntax — which would turn a
-- missing tenant into a 500 instead of an empty result. Comparing as text
-- makes "" match no row, which is the intended fail-closed behaviour.
--
-- ── What this migration does NOT do ──────────────────────────────────
--
-- It does not add authorization. This service has no authz client at all,
-- so within a tenant every principal can read and generate every manifest.
-- RLS is a tenant boundary, not a permission model, and conflating the two
-- would be the kind of overreach that makes a security control look
-- stronger than it is. Tracked separately.

ALTER TABLE evidence_manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_manifests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_manifests
    FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

-- The subquery is written out rather than hidden behind a SQL helper
-- function: a helper reads better, but it adds an indirection a reviewer of
-- a security policy has to chase, and the obvious way to make such a helper
-- perform (SECURITY DEFINER) is a well-known way to bypass the very policy
-- it supports.
ALTER TABLE manifest_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE manifest_records FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON manifest_records
    FOR ALL
    USING (manifest_id IN (SELECT manifest_id FROM evidence_manifests))
    WITH CHECK (manifest_id IN (SELECT manifest_id FROM evidence_manifests));

-- idx_manifest_records_manifest (migration 000001) already covers the
-- lookup this policy performs, so no additional index is needed.
