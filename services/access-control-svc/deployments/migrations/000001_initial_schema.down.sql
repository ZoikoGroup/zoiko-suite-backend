-- 000001_initial_schema.down.sql
-- Access Control Service — rollback initial schema

DROP TABLE IF EXISTS event_outbox;
DROP TABLE IF EXISTS role_permission_bundle_links;
DROP TABLE IF EXISTS permission_bundles;
DROP TABLE IF EXISTS roles;
