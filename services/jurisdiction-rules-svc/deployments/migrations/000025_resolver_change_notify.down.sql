-- 000025 down: removes the resolver change notifications. Replicas then fall back to the cache TTL and the local invalidate endpoint.
DROP TRIGGER IF EXISTS trg_notify_rollout ON jurisdiction_rollouts;
DROP TRIGGER IF EXISTS trg_notify_signing_key ON pack_signing_keys;
DROP TRIGGER IF EXISTS trg_notify_deployment ON pack_deployments;
DROP TRIGGER IF EXISTS trg_notify_pack_version ON jurisdiction_pack_versions;
DROP FUNCTION IF EXISTS jur_notify_resolver_change();
