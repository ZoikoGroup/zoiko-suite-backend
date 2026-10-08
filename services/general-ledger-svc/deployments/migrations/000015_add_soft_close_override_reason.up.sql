-- Migration: 000015_add_soft_close_override_reason.up.sql
--
-- ZS-SVC-B-001 ACC-14 §10.1 "soft-close override": when a period is in
-- SOFT_CLOSE (posting_policy RESTRICTED), ordinary posting is refused unless
-- the caller holds GL_SOFT_CLOSE_POSTING_OVERRIDE and provides a mandatory,
-- non-empty reason string. This reason must be persisted on the journal header
-- and emitted in the JournalCreated outbox event for audit traceability.
--
-- Unlike the control-account override (OverrideControlAccountRestriction),
-- which is a bare boolean with no reason, the soft-close override requires
-- "elevated finance role + reason + evidence" per the kernel standard
-- §10.1/§10.2. This column stores that reason.
--
-- Nullable: only set when override_soft_close=true and a reason is provided
-- at journal creation or posting time. Existing journals have NULL.

ALTER TABLE journal_headers
    ADD COLUMN soft_close_override_reason TEXT;