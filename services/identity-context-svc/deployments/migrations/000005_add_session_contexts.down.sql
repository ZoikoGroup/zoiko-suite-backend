-- Migration: 000005_add_session_contexts.down.sql
--
-- Dropping this table destroys session evidence that exists nowhere else once
-- the Redis copy has aged out. Reversible only in the sense that the schema
-- returns to its prior shape.

DROP POLICY IF EXISTS tenant_isolation_policy ON session_contexts;
DROP INDEX IF EXISTS idx_session_contexts_tenant_live;
DROP INDEX IF EXISTS idx_session_contexts_jti;
DROP INDEX IF EXISTS idx_session_contexts_principal;
DROP TABLE IF EXISTS session_contexts;
