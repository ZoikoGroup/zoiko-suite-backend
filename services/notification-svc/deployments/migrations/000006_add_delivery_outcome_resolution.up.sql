-- BIZ-10 Notification Wave 1: the doc's single most-emphasized control
-- (CTRL-020, T30, the shared error DELIVERY_OUTCOME_UNKNOWN, invariant
-- #25: "ambiguous external outcomes SHALL remain Pending/Unknown rather
-- than being guessed") had no home in this schema. A provider timeout
-- was force-classified as either "retry" or terminal "FAILED" — there
-- was no way to represent genuine ambiguity, so timeouts were being
-- guessed, which is exactly what invariant #25 exists to forbid.
--
-- PENDING_UNKNOWN is a new terminal-for-now status alongside PENDING/
-- SENT/FAILED. It behaves like a concluded status for retry purposes
-- (next_attempt_at is NULL — the existing worker never touches it again
-- automatically, exactly like the existing FindDueRetries query already
-- filters on status = 'PENDING' only) but is explicitly NOT SENT or
-- FAILED: nothing may be inferred about whether the message arrived
-- until ResolveDeliveryOutcome settles it one way or the other.
--
-- unknown_at / resolved_at / resolved_by_principal_id / resolution_note
-- are ResolveDeliveryOutcome's own evidence trail — who resolved an
-- ambiguous attempt, when, to what, and why.
--
-- template_id / template_version_id / rendered_content_hash close a
-- separate doc gap: BIZ-10's own evidence/lineage requirement ("template/
-- version, rendered hash") was not persisted even though the handler
-- already fetches and renders a governed BIZ-03 template version at send
-- time — the version actually used could not be proven after the fact.

ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_status_known;
ALTER TABLE notifications
    ADD CONSTRAINT notifications_status_known
    CHECK (status IN ('PENDING', 'SENT', 'FAILED', 'PENDING_UNKNOWN')) NOT VALID;

-- A PENDING_UNKNOWN row must say why the outcome is ambiguous, same
-- doctrine as the existing FAILED-requires-a-reason constraint.
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_pending_unknown_reason;
ALTER TABLE notifications
    ADD CONSTRAINT notifications_pending_unknown_reason
    CHECK (status <> 'PENDING_UNKNOWN' OR (failure_reason IS NOT NULL AND failure_reason <> '')) NOT VALID;

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS unknown_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS resolved_by_principal_id VARCHAR(255);
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS resolution_note TEXT;

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS template_id VARCHAR(255);
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS template_version_id VARCHAR(255);
-- sha256 hex digest of the rendered body actually sent — 64 hex chars.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS rendered_content_hash VARCHAR(64);

-- The existing due-retry index already filters status = 'PENDING' only
-- (migration 000004), so PENDING_UNKNOWN rows are correctly invisible to
-- the automatic retry worker with no change needed there — this index is
-- for the operator/reconciliation-side read (ListFailures-style queries,
-- Wave 2) that needs to find ambiguous rows to resolve.
CREATE INDEX IF NOT EXISTS idx_notifications_pending_unknown
    ON notifications (tenant_id, unknown_at)
    WHERE status = 'PENDING_UNKNOWN';
