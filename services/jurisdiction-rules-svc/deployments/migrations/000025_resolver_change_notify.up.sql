-- 000025_resolver_change_notify.up.sql
-- ZS-JUR-001 s23, s25, s29: a withdrawal, emergency block, supersession, deployment, rollback, signing-key revocation or rollout suspension reached
-- a running resolver only when its cache expired (RESOLVER_CACHE_TTL_SECONDS) or someone called the local invalidate endpoint on THAT replica.
-- With several replicas, an emergency block could keep being served by the others for the whole TTL.
--
-- These triggers publish a small NOTIFY on one channel whenever something a resolver caches changes; every replica LISTENs and drops its cache at
-- once. Triggers (not application code) so that every path is covered, including a direct SQL change by an operator. NOTIFY is delivered only
-- when the transaction COMMITS, so a replica never invalidates for a change that was rolled back. The payload carries no data of substance: it
-- only says which table changed.
--
-- NOTIFY is a wake-up, not a queue: a replica that was disconnected misses it, so the listener also invalidates on every (re)connect.

CREATE OR REPLACE FUNCTION jur_notify_resolver_change() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('jur_resolver_invalidate', TG_TABLE_NAME || ':' || TG_OP);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Pack version status (release, withdraw, block, unblock, supersede, certification).
DROP TRIGGER IF EXISTS trg_notify_pack_version ON jurisdiction_pack_versions;
CREATE TRIGGER trg_notify_pack_version AFTER UPDATE ON jurisdiction_pack_versions
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION jur_notify_resolver_change();

-- Deployments (a version becoming or ceasing to be served in a ring and region).
DROP TRIGGER IF EXISTS trg_notify_deployment ON pack_deployments;
CREATE TRIGGER trg_notify_deployment AFTER INSERT OR UPDATE ON pack_deployments
    FOR EACH ROW EXECUTE FUNCTION jur_notify_resolver_change();

-- Signing keys (a revoked key must stop verifying everywhere at once, JUR-NEG-04).
DROP TRIGGER IF EXISTS trg_notify_signing_key ON pack_signing_keys;
CREATE TRIGGER trg_notify_signing_key AFTER INSERT OR UPDATE ON pack_signing_keys
    FOR EACH ROW EXECUTE FUNCTION jur_notify_resolver_change();

-- Rollout status (the RESOLVER_REQUIRE_LAUNCHED gate: a suspension must take effect everywhere).
DROP TRIGGER IF EXISTS trg_notify_rollout ON jurisdiction_rollouts;
CREATE TRIGGER trg_notify_rollout AFTER UPDATE ON jurisdiction_rollouts
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION jur_notify_resolver_change();
