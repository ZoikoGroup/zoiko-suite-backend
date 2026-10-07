-- Reverse 000004.
--
-- It exists because the estate's convention is that every up has a down, not
-- because rolling back is safe: after this runs, catalogue events are once
-- again best-effort Kafka writes that a broker hiccup discards silently, and
-- two bundles on one role may once more share a code and so share — and
-- overwrite — a single grant in authorization-svc.
--
-- Unpublished rows are dropped with the table. Anything still in the backlog at
-- rollback time is an event that never reached its consumer, so drain the
-- outbox first if that matters:
--
--   SELECT count(*) FROM event_outbox WHERE published_at IS NULL;

DROP INDEX IF EXISTS idx_permission_bundle_defs_tenant_active;
DROP INDEX IF EXISTS idx_role_definitions_tenant_status;
DROP INDEX IF EXISTS idx_permission_bundle_defs_role_code;

DROP POLICY IF EXISTS outbox_tenant_isolation ON event_outbox;
DROP TABLE IF EXISTS event_outbox;
