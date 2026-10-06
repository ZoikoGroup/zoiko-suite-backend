-- +migrate Up
BEGIN;

-- LEG-05 (docs/architecture/original_doc) §1.1 and §2.2 state a cross-cutting
-- design rule directly: "Contract approval, execution, effectiveness,
-- performance, amendment, renewal, termination and dispute are orthogonal
-- facts. A single generic contract status is prohibited" / "one overloaded
-- status field is prohibited." This service had exactly one: `status`
-- (DRAFT/PENDING_APPROVAL/ACTIVE/EXPIRED/TERMINATED/SUSPENDED) conflated
-- approval, signature and effectiveness into a single enum.
--
-- Split per §2.2's named dimensions for this sub-service:
--   status            — the Instrument lifecycle dimension (LEG-05 §7's own
--                        list, specific to this sub-service).
--   signature_status  — the Signature dimension, independent of lifecycle:
--                        a contract can be APPROVED with signature still
--                        SENT, not yet COMPLETED.
--
-- `status`'s vocabulary changes from the old 6 values to:
-- DRAFT, REVIEW, APPROVED, EXECUTED, EFFECTIVE, TERMINATED, EXPIRED, ARCHIVED.
-- PENDING_APPROVAL -> REVIEW, ACTIVE -> EFFECTIVE (split from the old single
-- "activate" action that also meant "signed"), SUSPENDED dropped (unused by
-- any code path; not part of LEG-05's named lifecycle).
--
-- Amendment and renewal are NOT new status values: LEG-05 lists them in its
-- narrative lifecycle text, but modeling them as additional statuses would
-- mean a still-EFFECTIVE, still-binding contract stops reading as EFFECTIVE
-- the moment it is amended, which is wrong. They are recorded as events
-- (amended_at/renewed_at below) plus a new immutable contract_versions row
-- (LEG-CTRL "Executed legal instruments are immutable evidence... create
-- linked superseding events rather than in-place edits") — the contract
-- itself stays EFFECTIVE throughout.
ALTER TABLE contracts DROP CONSTRAINT IF EXISTS contracts_status_known;
ALTER TABLE contracts
    ADD CONSTRAINT contracts_status_known
    CHECK (status IN ('DRAFT','REVIEW','APPROVED','EXECUTED','EFFECTIVE','TERMINATED','EXPIRED','ARCHIVED')) NOT VALID;

ALTER TABLE contracts ADD COLUMN IF NOT EXISTS signature_status TEXT NOT NULL DEFAULT 'NOT_REQUESTED';
ALTER TABLE contracts
    ADD CONSTRAINT contracts_signature_status_known
    CHECK (signature_status IN
        ('NOT_REQUESTED','SENT','PARTIALLY_SIGNED','COMPLETED','DECLINED','EXPIRED','PENDING_UNKNOWN','VOIDED')
    ) NOT VALID;

-- Per-command attribution, replacing the single signed_at/signed_by pair
-- that conflated "approved" and "signed" into one fact.
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS submitted_at TIMESTAMPTZ;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS submitted_by TEXT;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS approved_by TEXT;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS signature_sent_at TIMESTAMPTZ;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS executed_at TIMESTAMPTZ;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS effective_at TIMESTAMPTZ;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS amended_at TIMESTAMPTZ;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS amended_by TEXT;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS renewed_at TIMESTAMPTZ;
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS renewed_by TEXT;

-- An approved-for-signature version is immutable (LEG-05 §7.1, invariant 1):
-- once APPROVED, CreateVersion (the general-purpose edit path) must refuse,
-- and only Amend/Renew — each of which records its own distinct attribution
-- above and its own evidence row in contract_versions — may change a
-- protected field again, and only once the contract has reached EFFECTIVE.
-- Enforced in the application layer (handler.go), same as every other
-- forward-only lifecycle in this codebase; this comment documents the rule
-- the code implements, the CHECK constraints above document the vocabulary.

-- FORCE, matching board-resolutions-svc's migration 000002 and this file's
-- own package-level doc comment (store/pg_store.go) describing the same
-- owner-bypasses-RLS class of bug already found and fixed here via explicit
-- tenant_id predicates. FORCE closes the remaining gap: the policy itself
-- was never actually applying to this connection.
ALTER TABLE contracts FORCE ROW LEVEL SECURITY;
ALTER TABLE contract_versions FORCE ROW LEVEL SECURITY;

COMMIT;
