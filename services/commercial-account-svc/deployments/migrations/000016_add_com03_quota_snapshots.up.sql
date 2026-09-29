-- 000016_add_com03_quota_snapshots.up.sql
-- Gap-remediation wave for ZS-SVC-Q-001 COM-03, following a skeptical
-- re-audit that found EvaluateCapability never consulted actual usage (a
-- request alone under the static limit was always allowed regardless of
-- how much quota the organization had already consumed), no automatic
-- entitlement invalidation on subscription change, no dunning->restriction
-- wiring, and no persisted EntitlementSnapshot despite the doc naming one.
-- This migration's part: the explicit capability<->meter binding, and the
-- append-only evidence table for computed decisions.

-- ── Capability<->meter binding (explicit, never inferred) ─────────────────
--
-- price_version_capabilities predates meter_definitions (migration 000006
-- vs 000011), so — same as price_components.meter_key/meter_version — this
-- is a plain nullable pair with no DB-level FK; registration is checked at
-- SubmitForApproval time in application code (loadRegisteredMetersForCapabilities),
-- exactly like a METERED price component already is.
ALTER TABLE price_version_capabilities
    ADD COLUMN meter_key     VARCHAR(128),
    ADD COLUMN meter_version INT CHECK (meter_version >= 1);

ALTER TABLE price_version_capabilities
    ADD CONSTRAINT price_version_capabilities_meter_pair CHECK ((meter_key IS NULL) = (meter_version IS NULL));

-- ── Entitlement snapshot (append-only evidence, never a cache the live
--    decision reads from) ──────────────────────────────────────────────────
--
-- EvaluateCapability remains the one live decision function, always called
-- fresh. entitlement_snapshots exists only to answer "what was this org's
-- entitlement at time T" after the fact — written whenever a decision is
-- explicitly captured (CreateSnapshot) or automatically recomputed
-- (a boundary-driven subscription change, a dunning-driven restriction).
CREATE TABLE entitlement_snapshots (
    snapshot_id              TEXT         PRIMARY KEY
        CHECK (snapshot_id ~ '^cesn_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id            UUID         NOT NULL,
    capability_key               VARCHAR(128) NOT NULL,
    outcome                        VARCHAR(16)  NOT NULL,
    limit_value                      BIGINT,
    limit_unit                        VARCHAR(32),
    subscription_outcome                VARCHAR(16)  NOT NULL,
    restriction_outcome                   VARCHAR(16),
    applied_restriction_id                  TEXT,
    policy_version                            INT,
    subscription_id                             TEXT,
    reason                                        TEXT         NOT NULL,
    decided_at                                     TIMESTAMPTZ  NOT NULL,
    created_at                                      TIMESTAMPTZ  NOT NULL,
    created_by_principal_id                          VARCHAR(255) NOT NULL
);

CREATE INDEX idx_entitlement_snapshots_org_key ON entitlement_snapshots (organization_id, capability_key, created_at DESC);

CREATE TRIGGER trg_entitlement_snapshots_immutable
    BEFORE UPDATE OR DELETE ON entitlement_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE entitlement_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE entitlement_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY entitlement_snapshots_read ON entitlement_snapshots FOR SELECT
    USING (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker')
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
-- Writes are platform/system authority only (CreateSnapshot is a delegated
-- operator command; the boundary worker and dunning-driven recompute are
-- both system-initiated) — never a plain tenant fabricating its own
-- evidence, unlike the read policy above.
CREATE POLICY entitlement_snapshots_insert ON entitlement_snapshots FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker'));
-- UPDATE/DELETE granted (seller/boundary_worker only) so an attempt reaches
-- the immutability trigger above rather than being silently filtered to
-- zero rows by RLS alone (same idiom as every other evidence table in this
-- service — see e.g. migration 000012's platform_commercial_invoices).
CREATE POLICY entitlement_snapshots_update ON entitlement_snapshots FOR UPDATE
    USING (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker'))
    WITH CHECK (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker'));
CREATE POLICY entitlement_snapshots_delete ON entitlement_snapshots FOR DELETE
    USING (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker'));

-- ── Dunning -> restriction linkage ────────────────────────────────────────
--
-- Nullable: only set once a case escalates to RESTRICTED/SUSPENDED and a
-- CommercialRestriction is applied on its behalf, so StopDunning knows
-- exactly which restriction (if any) to lift.
ALTER TABLE dunning_cases ADD COLUMN applied_restriction_id TEXT REFERENCES commercial_restrictions (restriction_id);
