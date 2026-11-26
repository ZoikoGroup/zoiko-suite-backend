-- 000006_add_release_reconciliation_procedure_ref.up.sql
--
-- Gap 2 fix (2026-11-19 audit): Release.State could be flipped to
-- INCIDENT_RESTRICTED or DISABLED with the same single permission and
-- optional free-text reason as a routine GA/BETA/PILOT/INTERNAL promotion —
-- none of the rigor kill-switch-registry-svc requires on every engage
-- (doc7 §32.1). The handler now requires a non-empty reason AND
-- reconciliation_procedure_ref for those two emergency-style transitions
-- only; this column is nullable at the schema level because it stays
-- genuinely optional for the four routine states.

ALTER TABLE releases
    ADD COLUMN reconciliation_procedure_ref TEXT;

-- Backfill any pre-existing emergency-state row (written before this gate
-- existed) with a placeholder, so the CHECK constraint below can be added
-- without failing validation against history this migration did not create
-- and cannot retroactively know the real procedure for. This is a schema
-- migration, not a claim that a real reconciliation procedure existed for
-- these historical rows — same doctrine as migration 000005's backfill.
UPDATE releases
SET reconciliation_procedure_ref = 'UNKNOWN-PRE-GAP2-MIGRATION',
    reason = COALESCE(NULLIF(reason, ''), 'unknown (pre-existing row, backfilled by migration 000006)')
WHERE state IN ('INCIDENT_RESTRICTED', 'DISABLED')
  AND (reconciliation_procedure_ref IS NULL OR reconciliation_procedure_ref = ''
       OR reason IS NULL OR reason = '');

-- Defense in depth, same spirit as the handler-layer release-state
-- Valid() check already closing a fail-open gap for pre-existing rows
-- (see pg_store.go's ResolveCapability doc comment): any row inserted
-- straight into Postgres, bypassing the handler, still cannot claim an
-- emergency state with no reconciliation_procedure_ref or reason.
ALTER TABLE releases
    ADD CONSTRAINT releases_emergency_requires_reconciliation
        CHECK (
            state NOT IN ('INCIDENT_RESTRICTED', 'DISABLED')
            OR (reconciliation_procedure_ref IS NOT NULL AND reconciliation_procedure_ref <> ''
                AND reason IS NOT NULL AND reason <> '')
        );
