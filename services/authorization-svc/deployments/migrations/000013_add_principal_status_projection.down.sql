-- Migration: 000013_add_principal_status_projection.down.sql
--
-- Drops the projection. NOT revertible on a running service: since the 7 Oct
-- 2026 governance pass FindPrincipalStatus fails CLOSED on a missing relation
-- (ZS-IAM-001), so once this runs every evaluation answers 503 until the
-- service is rolled back to a build older than that pass. A missing table used
-- to answer ACTIVE, which silently admitted every suspended principal.
--
-- Suspensions published while this migration is reverted are lost, not queued:
-- re-applying it starts from an empty projection, so identity-context-svc has
-- to republish or the operator re-suspends. The role assignments themselves are
-- untouched by either direction — this table never held them.

BEGIN;

DROP TABLE IF EXISTS principal_status_projection;

COMMIT;
