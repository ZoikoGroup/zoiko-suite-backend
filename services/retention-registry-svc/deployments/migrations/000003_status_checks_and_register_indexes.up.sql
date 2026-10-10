-- retention-registry-svc: status constraints and register indexes.
--
-- WHY THERE IS NO ROW-LEVEL SECURITY HERE, DELIBERATELY.
--
-- Both tables carry a nullable tenant_id, and the reflex for that in this estate
-- is ENABLE + FORCE ROW LEVEL SECURITY with a tenant_isolation policy keyed on
-- current_setting('app.tenant_id'). That is the right control almost everywhere
-- and it is the wrong one here. It was written, then removed before shipping,
-- because it would have broken the service outright rather than protecting it:
--
--  1. This store never sets app.tenant_id. There is no withRLS/withTenantTx
--     wrapper; every query goes straight to the pool. With FORCE on and the GUC
--     unset, the policy matches only rows whose tenant_id IS NULL, so every
--     tenant-scoped policy and hold becomes invisible and every tenant-scoped
--     INSERT fails its WITH CHECK. Since this service is not in the per-service
--     app-role set it connects as zoiko_app -- NOSUPERUSER NOBYPASSRLS -- so the
--     policy really would apply. This is not a theoretical break.
--
--  2. More fundamentally, GET /v1/retention/resolve is unauthenticated by
--     design and takes tenant_id as a QUERY PARAMETER: it is the check every
--     other service makes before deleting, exporting or migrating a record, and
--     service X legitimately asks it about tenant T. A policy keyed on the
--     CONNECTION's tenant cannot express "answer about the tenant named in the
--     request". Setting the GUC to the queried tenant would make the policy
--     decorative -- it would authorise whatever it was told to authorise.
--
-- So the isolation on this service is the explicit `tenant_id` predicate in each
-- store query, which is the control the estate's own isolation suites describe as
-- "the one that does not depend on the role being right". Two paths did not have
-- it -- reading and releasing a legal hold matched on legal_hold_id alone, so any
-- caller with an id could read or release another tenant's hold -- and those are
-- fixed in Go in the same change as this migration, with tests.
--
-- If RLS is wanted here later it is a real piece of work, not a migration:
-- resolve needs a different contract (an authenticated caller, or an explicit
-- platform-scope role), and the store needs the transaction wrapper.

-- ── status vocabularies, which were comments rather than constraints ─────────
--
-- policy_status and hold_status are documented in domain/types.go as closed sets
-- and were plain VARCHAR, so any string persisted -- and a row whose status is
-- neither ACTIVE nor RELEASED is invisible to every status filter without being
-- either of them.
--
-- NOT VALID: every existing row was written through a handler that sets a
-- literal, so there is nothing to repair, and a migration that can fail on
-- historical data is a migration that blocks a deploy. The constraint binds every
-- future write regardless.
ALTER TABLE retention_policies
    ADD CONSTRAINT retention_policies_status_check
    CHECK (policy_status IN ('ACTIVE', 'SUPERSEDED', 'RETIRED')) NOT VALID;

ALTER TABLE legal_holds
    ADD CONSTRAINT legal_holds_status_check
    CHECK (hold_status IN ('ACTIVE', 'RELEASED')) NOT VALID;

-- ── indexes for the register reads added alongside this migration ────────────
--
-- Both new list endpoints filter on (tenant_id OR NULL) and order by recency;
-- legal holds additionally sort ACTIVE first, because an active hold is what
-- blocks a deletion now and a released one is history.
CREATE INDEX IF NOT EXISTS idx_retention_policies_tenant_effective
    ON retention_policies (tenant_id, effective_from DESC);

CREATE INDEX IF NOT EXISTS idx_legal_holds_tenant_status_started
    ON legal_holds (tenant_id, hold_status, started_at DESC);
