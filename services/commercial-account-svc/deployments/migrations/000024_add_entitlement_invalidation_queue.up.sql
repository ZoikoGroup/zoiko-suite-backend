-- 000024_add_entitlement_invalidation_queue.up.sql
-- COM-03 Entitlement, part 3b (ZS-SVC-Q-001 §4.3; SPEC_DEVIATIONS.md A5):
-- PublishEntitlementPolicy's missing per-tenant EntitlementSnapshotChanged
-- signal.
--
-- entitlement_policy_versions (migration 000010) is a single, global,
-- platform-scoped ended-access grace policy -- it names no organization and
-- no capability. The only organizations a given policy version can actually
-- change the answer for are those with no live subscription whose most
-- recent one has already ended (loadMostRecentlyEnded); an organization with
-- a live subscription is unaffected by it regardless of its content.
--
-- PublishEntitlementPolicy runs on the seller plane with no tenant scope
-- (withSellerPlane), and `subscriptions` (migration 000007) carries only a
-- bare tenant_isolation_policy -- deliberately, with no seller or
-- boundary-worker bypass of its own (see the declareOrgForSellerPlane
-- comment in internal/store/com04_usage_store.go, which documents this same
-- fact for a different COM-04 read). Widening that policy would let the
-- seller plane read live tenant subscription data in general, a far bigger
-- surface than this one feature needs. Instead:
--
--   1. ended_subscription_index is a new, narrow, durable index: one row
--      per organization whose most recent subscription currently has an
--      ends_at set, kept current by a trigger on `subscriptions` itself
--      (AFTER INSERT OR UPDATE OF ends_at) -- the exact column setEndsAt
--      already maintains for every real lifecycle transition this service
--      has (CancelNow, a scheduled cancellation taking effect, trial/term
--      expiry, reactivation clearing it back to NULL; see
--      internal/store/com02_subscription_store.go's setEndsAt and its
--      callers). The write happens inside the owning tenant's own
--      transaction -- the same one that already calls setEndsAt -- so it is
--      RLS-legal without any cross-tenant read ever happening.
--   2. PublishEntitlementPolicy's own (seller-plane) transaction reads this
--      index -- never `subscriptions` directly -- to enqueue one
--      entitlement_invalidation_queue row per organization whose indexed
--      ends_at is at or before the policy's own effective_from.
--   3. A worker shaped exactly like subscription_boundary_queue's
--      (ProcessNextBoundary) drains that queue and emits the real
--      per-tenant entitlement_snapshot.changed event through the existing
--      recomputeEntitlementsBatchTx path -- the same event CreateSnapshot,
--      RecomputeEntitlements and the boundary worker's own B2 remediation
--      already produce.

-- ── 1. Ended-subscription index (tenant-written, seller-read) ────────────
CREATE TABLE ended_subscription_index (
    organization_id   UUID         PRIMARY KEY,
    subscription_id   TEXT         NOT NULL REFERENCES subscriptions (subscription_id),
    ends_at           TIMESTAMPTZ  NOT NULL,
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE FUNCTION index_subscription_ends_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.ends_at IS NULL THEN
        -- No longer ended (a brand-new subscription just started for this
        -- organization, or an existing one was reactivated): the
        -- organization is no longer governed by the ended-access policy at
        -- all, so a stale row here would wrongly mark it as affected.
        DELETE FROM ended_subscription_index WHERE organization_id = NEW.organization_id;
    ELSE
        INSERT INTO ended_subscription_index (organization_id, subscription_id, ends_at, updated_at)
        VALUES (NEW.organization_id, NEW.subscription_id, NEW.ends_at, now())
        ON CONFLICT (organization_id) DO UPDATE
            SET subscription_id = EXCLUDED.subscription_id, ends_at = EXCLUDED.ends_at, updated_at = now();
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_subscriptions_index_ends_at
    AFTER INSERT OR UPDATE OF ends_at ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION index_subscription_ends_at();

-- Read: the seller plane (PublishEntitlementPolicy) needs every
-- organization; a tenant transaction sees only its own row -- the same
-- read shape as this service's other seller/tenant hybrid tables
-- (commercial_restrictions, price_migration_offers). Write: the trigger
-- above is the only writer and it always runs inside the owning
-- organization's own transaction, so a plain tenant-scoped check is enough;
-- the seller plane never writes this table directly, so it gets no write
-- branch.
ALTER TABLE ended_subscription_index ENABLE ROW LEVEL SECURITY;
ALTER TABLE ended_subscription_index FORCE ROW LEVEL SECURITY;
CREATE POLICY ended_subscription_index_read ON ended_subscription_index FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY ended_subscription_index_tenant_insert ON ended_subscription_index FOR INSERT
    WITH CHECK (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY ended_subscription_index_tenant_update ON ended_subscription_index FOR UPDATE
    USING (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY ended_subscription_index_tenant_delete ON ended_subscription_index FOR DELETE
    USING (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

-- Backfill for organizations whose subscription already ended before this
-- index existed. Idempotent and safely re-runnable: it recomputes current
-- truth from `subscriptions` and upserts (ON CONFLICT), rather than a
-- blind one-time copy, while still following this service's only existing
-- backfill convention -- a migration-embedded INSERT, run by the migration
-- role, which reads `subscriptions` without RLS applying to it (same
-- posture as 000009's subscription_boundary_queue backfill).
INSERT INTO ended_subscription_index (organization_id, subscription_id, ends_at, updated_at)
SELECT DISTINCT ON (s.organization_id) s.organization_id, s.subscription_id, s.ends_at, now()
  FROM subscriptions s
 WHERE s.ends_at IS NOT NULL
   AND NOT EXISTS (
       SELECT 1 FROM subscriptions s2
        WHERE s2.organization_id = s.organization_id
          AND (s2.ends_at IS NULL OR s2.ends_at > now()))
 ORDER BY s.organization_id, s.ends_at DESC
ON CONFLICT (organization_id) DO UPDATE
    SET subscription_id = EXCLUDED.subscription_id, ends_at = EXCLUDED.ends_at, updated_at = now();

-- ── 2. Entitlement invalidation queue (platform-only) ─────────────────────
--
-- Mirrors subscription_boundary_queue's shape exactly: a status/attempt/
-- processed_at work queue, claimed with SKIP LOCKED, drained by a worker
-- that processes exactly one due item per transaction. Unlike the boundary
-- queue this table is never tenant-owned data -- no organization ever reads
-- its own rows here -- so its RLS has no per-organization branch at all:
-- only the platform planes that write (seller, PublishEntitlementPolicy)
-- and drain (boundary_worker, the new worker below) it may see it.
CREATE TABLE entitlement_invalidation_queue (
    invalidation_id    UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id    UUID         NOT NULL,
    policy_version     INT          NOT NULL REFERENCES entitlement_policy_versions (policy_version),
    due_at             TIMESTAMPTZ  NOT NULL,
    next_attempt_at    TIMESTAMPTZ  NOT NULL,
    attempts           INT          NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error         TEXT,
    status             VARCHAR(16)  NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'DONE', 'FAILED')),
    outcome            VARCHAR(32),
    processed_at       TIMESTAMPTZ,
    CONSTRAINT invalidation_done_evidence CHECK ((status = 'PENDING') = (processed_at IS NULL)),
    CONSTRAINT invalidation_one_per_target UNIQUE (organization_id, policy_version)
);

CREATE INDEX idx_entitlement_invalidation_due ON entitlement_invalidation_queue (next_attempt_at) WHERE status = 'PENDING';

ALTER TABLE entitlement_invalidation_queue ENABLE ROW LEVEL SECURITY;
ALTER TABLE entitlement_invalidation_queue FORCE ROW LEVEL SECURITY;
CREATE POLICY entitlement_invalidation_queue_access ON entitlement_invalidation_queue FOR ALL
    USING (current_setting('app.commercial_plane', true) IN ('boundary_worker', 'seller'))
    WITH CHECK (current_setting('app.commercial_plane', true) IN ('boundary_worker', 'seller'));
