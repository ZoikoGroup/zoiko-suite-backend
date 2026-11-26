-- Migration: 000011_tolerance_policy_versions.up.sql
--
-- SPEC_DEVIATIONS A5 (the tolerance/matching-policy gap B15 deliberately left
-- open): MatchEntry compared a target journal's intercompany-line amount to
-- the pair's expected counterparty amount within a fixed `0.001` big.Rat
-- literal, hardcoded in internal/handler/handler.go, with no governance and
-- no version history. This migration does not change that number — the spec
-- gives no different tolerance and none is invented here — it turns it into
-- a governed, versioned object: who may publish a tolerance, when it takes
-- effect, and which version a historical match attempt was evaluated under.
--
-- Shape mirrors commercial-account-svc's entitlement_policy_versions
-- (its migration 000010): an explicit, caller-chosen version number as part
-- of the primary key (never a SERIAL, so a replayed publish for the same
-- version is a natural conflict/no-op rather than a race on an auto-assigned
-- number), effective-dated, immutable once written (reject_immutable_row,
-- already defined in this service by migration 000007), reason +
-- created_by_principal_id for governance evidence, readable by anyone,
-- published only by an authorized principal (checked in the handler, like
-- every other command in this service).
--
-- Unlike entitlement_policy_versions this table is tenant-scoped
-- (tenant_id, policy_version): this service has no cross-tenant seller
-- plane — every table here is tenant-partitioned (see pg_store.go's
-- withRLS) — so there is no single cross-tenant tolerance to govern, only
-- each tenant's own.
--
-- A tenant that has never published a version still matches at the fixed
-- pre-governance 0.001 (resolved in Go as tolerance_version 0, never a
-- stored row): publishing this migration changes no tenant's match outcome
-- until that tenant actually opts in by publishing a version.
CREATE TABLE intercompany_tolerance_policy_versions (
    tenant_id               VARCHAR(255) NOT NULL,
    policy_version          INT          NOT NULL CHECK (policy_version >= 1),
    tolerance               NUMERIC      NOT NULL CHECK (tolerance >= 0),
    effective_from          TIMESTAMPTZ  NOT NULL,
    reason                  TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    created_by_principal_id VARCHAR(255) NOT NULL,
    PRIMARY KEY (tenant_id, policy_version)
);

-- Answers ResolveTolerancePolicy's lookup: the latest version effective by a
-- given instant, for a tenant.
CREATE INDEX idx_intercompany_tolerance_policy_effective
    ON intercompany_tolerance_policy_versions (tenant_id, effective_from DESC, policy_version DESC);

CREATE TRIGGER trigger_tolerance_policy_versions_immutable
    BEFORE UPDATE OR DELETE ON intercompany_tolerance_policy_versions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE intercompany_tolerance_policy_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE intercompany_tolerance_policy_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY intercompany_tolerance_policy_tenant_isolation ON intercompany_tolerance_policy_versions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- intercompany_match_attempts (migration 000007) now records which tolerance
-- version was actually evaluated. 0 for every attempt recorded before this
-- column existed, and for any attempt a tenant with no published policy
-- makes afterward: the fixed pre-governance 0.001, not a stored version —
-- never retroactively assigned to a "real" version number it was never
-- actually evaluated under.
ALTER TABLE intercompany_match_attempts ADD COLUMN tolerance_version INT NOT NULL DEFAULT 0;
