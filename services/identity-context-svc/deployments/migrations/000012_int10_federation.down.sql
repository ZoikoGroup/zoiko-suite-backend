-- Rollback of 000012_int10_federation.up.sql
DROP TABLE IF EXISTS federation_token_uses;
DROP TABLE IF EXISTS federation_login_log;
DROP TABLE IF EXISTS federation_links;
DROP TABLE IF EXISTS federation_key_sets;
DROP TABLE IF EXISTS federation_connection_events;
DROP TABLE IF EXISTS federation_connection_versions;
DROP TABLE IF EXISTS federation_connections;
DROP FUNCTION IF EXISTS federation_evidence_guard();
DROP FUNCTION IF EXISTS federation_version_guard();
