-- +migrate Up
BEGIN;

-- LEG-07 (docs/architecture/original_doc) §9 names the lifecycle Planned ->
-- Active -> Due -> In Progress -> Satisfied/Waived/Breached/Disputed/
-- Superseded, commands CreateObligation/ValidateExtractedObligation/
-- Schedule/Complete/Waive/RecordBreach/Dispute/Supersede, and a mandatory
-- invariant: "AI extraction cannot activate obligation without validation."
-- This service had a 5-value status (PENDING/IN_PROGRESS/FULFILLED/
-- BREACHED/WAIVED) reachable only through CreateObligation (direct) and a
-- single unvalidated UpdateObligation that could set status to anything —
-- no candidate state, no validation gate, no Schedule/Waive/RecordBreach/
-- Dispute/Supersede command, and no due-date provenance (source clause,
-- trigger, calculation method) despite §9.1's invariant that due dates must
-- retain exactly that.
ALTER TABLE obligations DROP CONSTRAINT IF EXISTS obligations_status_known;
ALTER TABLE obligations
    ADD CONSTRAINT obligations_status_known
    CHECK (status IN
        ('CANDIDATE','PLANNED','ACTIVE','DUE','IN_PROGRESS',
         'SATISFIED','WAIVED','BREACHED','DISPUTED','SUPERSEDED')
    ) NOT VALID;

-- CANDIDATE precedes PLANNED only for AI-extracted obligations — §9.1:
-- "AI extraction cannot activate obligation without validation." A
-- manually-created obligation has nothing to validate and starts at PLANNED
-- directly; extracted_by_ai is what distinguishes the two creation paths.
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS extracted_by_ai BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS validated_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS validated_by TEXT;

-- Due-date provenance (§9.1: "Obligation due dates retain source clause,
-- trigger and calculation method"). Populated by Schedule, which is this
-- service's due-date certification step — §9's own failure semantics:
-- "trigger/date ambiguity blocks due-date certification" is enforced by
-- requiring both non-empty before Schedule succeeds.
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS source_clause_id TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS trigger_description TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS calculation_method TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS scheduled_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS scheduled_by TEXT;

ALTER TABLE obligations ADD COLUMN IF NOT EXISTS due_marked_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS due_marked_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS in_progress_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS in_progress_by TEXT;

-- Waiver is distinct from satisfaction and requires documented authority
-- (§9.1) — waiver_authority_reference is that documentation, required
-- non-empty by the application layer before Waive succeeds.
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS satisfied_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS satisfied_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS waived_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS waived_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS waiver_authority_reference TEXT;

-- Breach does NOT automatically create financial accrual, payment or legal
-- remedy (§9.1) — this service records the fact only; target domains act
-- separately, which is enforced simply by this service never calling out to
-- any of them from RecordBreach.
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS breached_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS breached_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS breach_note TEXT;

ALTER TABLE obligations ADD COLUMN IF NOT EXISTS disputed_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS disputed_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS dispute_reason TEXT;

ALTER TABLE obligations ADD COLUMN IF NOT EXISTS superseded_by TEXT;

-- Tenant isolation: this table's RLS policy was ENABLE only, never FORCE,
-- and GetObligation/UpdateObligation/FulfillObligation(now Complete) read
-- and wrote by id alone with no explicit tenant_id predicate of their own.
-- This pool connects as the Postgres superuser/table owner (same pattern
-- as every other service on this platform), and Postgres unconditionally
-- exempts a table's owner from RLS unless FORCE ROW LEVEL SECURITY is set
-- — so this policy never applied to a single query this service made.
-- FORCE closes that; the store's own explicit tenant_id predicates (added
-- alongside this migration) are the belt to this policy's braces, the same
-- defence-in-depth already applied to board-resolutions-svc,
-- contract-lifecycle-svc and clause-template-svc after the identical bug
-- was found live in each.
ALTER TABLE obligations FORCE ROW LEVEL SECURITY;

COMMIT;
