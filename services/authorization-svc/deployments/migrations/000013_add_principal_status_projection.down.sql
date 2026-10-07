-- Migration: 000013_add_principal_status_projection.down.sql
--
-- Drops the projection. POST /v1/authorize's layer-0 gate reads "no row means
-- ACTIVE", so after this runs every principal evaluates as active again and no
-- other layer changes behaviour — FindPrincipalStatus answers ACTIVE on a
-- missing relation rather than failing the evaluation, which is what keeps this
-- migration revertible on a live service.
--
-- Suspensions published while this migration is reverted are lost, not queued:
-- re-applying it starts from an empty projection, so identity-context-svc has
-- to republish or the operator re-suspends. The role assignments themselves are
-- untouched by either direction — this table never held them.

BEGIN;

DROP TABLE IF EXISTS principal_status_projection;

COMMIT;
