-- 000013_extended_lifecycle.up.sql
-- Obligations Service — WFC-05 consolidation, step 1 (additive, non-breaking).
--
-- Doc tracker row 78 / ZS-SVC-R-001 §0: obligation tracking is duplicated
-- across obligations-svc, obligation-tracking-svc and filing-tracker-svc with
-- non-identical schemas, in violation of the single-owner doctrine ("Do not
-- create separate ... workflow engines unless a documented exception is
-- approved by Platform Architecture" — no such exception exists in this
-- repo). This is the backend half of consolidating onto obligations-svc as
-- the one canonical WFC-05 "Obligation, Deadline & Escalation Service".
--
-- Scope of this migration: ADD the columns and status values needed to
-- represent what obligation-tracking-svc (contract-derived obligations) and
-- filing-tracker-svc (filing submissions) each track today, under one
-- obligations-svc row. Nothing here changes behavior for an existing row or
-- an existing caller:
--   - lifecycle_model discriminates LEGACY (today's OPEN/IN_PROGRESS/
--     OVERDUE/CLOSED machine, untouched, default for every existing and
--     future row created via the pre-existing CreateObligation path) from
--     EXTENDED (the new WFC-05 9-state machine, opt-in, used only by the
--     new contract/candidate creation path added alongside this migration).
--   - clock_class is new data, defaulted for existing rows rather than
--     inferred, since inferring HARD_LEGAL vs REGULATORY from
--     obligation_source_type would be a guess this migration should not
--     make silently.
--   - Every other new column is nullable: unused unless the EXTENDED path
--     populates it.

BEGIN;

-- ── extend the status CHECK (000003's obligations_status_known) ──────────
-- It only ever allowed the LEGACY 4 values; the EXTENDED machine's status
-- vocabulary is new data, not a code change, same doctrine as every other
-- VARCHAR tag on this table — but a CHECK constraint still has to be told.
ALTER TABLE obligations DROP CONSTRAINT IF EXISTS obligations_status_known;
ALTER TABLE obligations
    ADD CONSTRAINT obligations_status_known
    CHECK (obligation_status IN (
        'OPEN', 'IN_PROGRESS', 'OVERDUE', 'CLOSED',
        'CANDIDATE', 'PLANNED', 'ACTIVE', 'DUE_SOON', 'DUE',
        'FULFILLED', 'WAIVED', 'NOT_APPLICABLE', 'SUPERSEDED', 'BREACHED', 'DISPUTED'
    )) NOT VALID;

-- ── lifecycle_model + clock_class ──────────────────────────────────────────
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS lifecycle_model VARCHAR(16) NOT NULL DEFAULT 'LEGACY';
ALTER TABLE obligations DROP CONSTRAINT IF EXISTS chk_obligations_lifecycle_model;
ALTER TABLE obligations ADD CONSTRAINT chk_obligations_lifecycle_model
    CHECK (lifecycle_model IN ('LEGACY', 'EXTENDED'));

-- Spec §7.1 "Clock class": HARD_LEGAL | REGULATORY | CONTRACTUAL |
-- INTERNAL_SLA | ADVISORY. Data only (same doctrine as obligation_type) — a
-- new clock class never requires a code change, only the escalation/warning
-- policy that reads it.
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS clock_class VARCHAR(32) NOT NULL DEFAULT 'REGULATORY';

-- ── AI-extraction / validation gate (LEG-07 §9.1, obligation-tracking-svc
--    parity): "AI extraction cannot activate obligation without validation".
--    CANDIDATE precedes PLANNED only on this path. ─────────────────────────
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS extracted_by_ai BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS validated_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS validated_by TEXT;

-- ── due-date provenance (§9.1 "trigger/date ambiguity blocks due-date
--    certification") ─────────────────────────────────────────────────────
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS source_clause_id TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS trigger_description TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS calculation_method TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS scheduled_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS scheduled_by TEXT;

-- ── deadline-lifecycle marks (spec §7.2's PLANNED/ACTIVE/DUE_SOON/DUE
--    timestamps; OVERDUE is computed, not marked, so it has no *_at here) ──
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS due_soon_marked_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS due_marked_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS due_marked_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS in_progress_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS in_progress_by TEXT;

-- ── resolution evidence: FULFILLED | WAIVED | NOT_APPLICABLE each require
--    their own evidence, never a shared "closed_at" — a waiver is not a
--    satisfaction and must not look like one in the data. ─────────────────
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS fulfilled_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS fulfilled_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS fulfillment_evidence_ref TEXT;

ALTER TABLE obligations ADD COLUMN IF NOT EXISTS waived_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS waived_by TEXT;
-- Spec §7.1 "Waiver ... requires policy authority, reason code, approver and
-- evidence; cannot be a hidden admin toggle" — all three are mandatory at
-- the application layer when transitioning to WAIVED (enforced in Go, not a
-- DB CHECK, same doctrine as the status machine itself).
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS waiver_authority_reference TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS waiver_reason_code TEXT;

-- Spec §7.1 "NOT_APPLICABLE ... Decision evidence mandatory" — same
-- discipline as a waiver: this is a governed decision, not a silent flag.
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS not_applicable_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS not_applicable_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS not_applicable_reason TEXT;

-- Breach/dispute are facts, recorded without side effects elsewhere
-- (obligation-tracking-svc precedent: "Breach status does not automatically
-- create financial accrual, payment or legal remedy; target domains act
-- separately" — enforced simply by never calling out to another domain
-- from the transition that records one).
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS breached_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS breached_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS breach_note TEXT;

ALTER TABLE obligations ADD COLUMN IF NOT EXISTS disputed_at TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS disputed_by TEXT;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS dispute_reason TEXT;

-- Spec §7.1 "Supersession: recalculation/new governing rule creates
-- successor link; old due date remains reconstructable" — a real FK to the
-- successor row, not a free-text note, so the chain is queryable.
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS superseded_by_obligation_id UUID
    REFERENCES obligations(obligation_id);

-- Spec §7.1 "Obligation instance: created ... for a specific
-- subject/entity/jurisdiction" with an effective window, matching
-- obligation-tracking-svc's EffectiveFrom/EffectiveTo.
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS effective_from TIMESTAMPTZ;
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS effective_to TIMESTAMPTZ;

-- A CANDIDATE obligation is not yet real and must never be counted as an
-- open duty until validated; indexed since the validation worklist is a
-- real query ("show me every candidate awaiting review").
CREATE INDEX IF NOT EXISTS idx_obligations_candidates
    ON obligations (tenant_id, created_at)
    WHERE lifecycle_model = 'EXTENDED' AND obligation_status = 'CANDIDATE';

-- Query shape most of the above exists to support: "every EXTENDED
-- obligation due soon or overdue, by tenant", used by the (future) unified
-- scheduler in place of the per-service schedulers obligation-tracking-svc
-- and filing-tracker-svc each run today.
CREATE INDEX IF NOT EXISTS idx_obligations_extended_due
    ON obligations (tenant_id, due_date)
    WHERE lifecycle_model = 'EXTENDED';

-- ── filing_requirements: bring up to filing-tracker-svc's shape ──────────
-- filing-tracker-svc tracks a filing by (legal_entity_id, jurisdiction_id,
-- filing_authority, filing_type, period_key) with its own due_date,
-- independent of any one obligation's due_date, plus submission/
-- confirmation evidence. All additive; existing rows read these as NULL.
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS jurisdiction_id UUID;
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS period_key VARCHAR(64);
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS due_date TIMESTAMPTZ;
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS submission_reference TEXT;
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS submitted_at TIMESTAMPTZ;
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS submitted_by TEXT;
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS confirmation_reference TEXT;
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ;
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS rejection_reason TEXT;
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS notes TEXT;

-- 000003 also added filing_requirements_status_known, a narrower 4-value
-- CHECK (PENDING/SUBMITTED/ACCEPTED/REJECTED only) that 000012 never dropped
-- when it introduced chk_filing_requirements_status with the full 6-value
-- set — both constraints have applied to every insert/update since, so
-- IN_PREPARATION and WITHDRAWN were already silently impossible in practice.
-- Drop the stale, narrower one; chk_filing_requirements_status (extended
-- below) is the one source of truth for this column now.
ALTER TABLE filing_requirements DROP CONSTRAINT IF EXISTS filing_requirements_status_known;

-- Extend, not replace, the status CHECK — existing values stay legal.
-- filing-tracker-svc's SCHEDULED/DRAFT_READY map onto this service's
-- pre-filing states; CONFIRMED and OVERDUE are new terminal/urgency states
-- this table did not previously need since it had no independent due_date.
ALTER TABLE filing_requirements DROP CONSTRAINT IF EXISTS chk_filing_requirements_status;
ALTER TABLE filing_requirements ADD CONSTRAINT chk_filing_requirements_status
    CHECK (filing_status IN (
        'PENDING', 'IN_PREPARATION', 'SUBMITTED', 'ACCEPTED', 'REJECTED', 'WITHDRAWN',
        'SCHEDULED', 'DRAFT_READY', 'CONFIRMED', 'OVERDUE'
    ))
    NOT VALID;

CREATE INDEX IF NOT EXISTS idx_filing_requirements_jurisdiction
    ON filing_requirements (jurisdiction_id) WHERE jurisdiction_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_filing_requirements_period
    ON filing_requirements (obligation_id, period_key) WHERE period_key IS NOT NULL;

COMMIT;
