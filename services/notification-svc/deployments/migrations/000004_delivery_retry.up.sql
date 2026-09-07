-- Bounded retry for transient delivery failures.
--
-- 000003 gave the register somewhere to record that a failure was worth
-- re-attempting, and nothing re-attempted. A greylisted payslip notice, a relay
-- restarting, an identity-context-svc blip: each concluded FAILED on the first
-- try and stayed that way. The classification existed and was inert.
--
-- ── What "PENDING" now means ────────────────────────────────────────────────
--
-- No new status value. PENDING already means "delivery has not concluded", and
-- a notification awaiting another attempt has not concluded — so it stays
-- PENDING with next_attempt_at set, and reaches SENT or FAILED exactly once.
--
-- The alternative was a RETRYING status, which would have meant widening
-- notifications_status_known, updating every consumer's vocabulary, and
-- teaching the console a fourth state. It would also have made FAILED
-- ambiguous for as long as the rollout took: a FAILED row would mean either
-- "terminally failed" or "failed once, from an older binary that had no
-- RETRYING". PENDING carries the meaning already.
--
--   PENDING, next_attempt_at IS NOT NULL  → will be attempted again
--   PENDING, next_attempt_at IS NULL      → in flight right now
--   SENT                                  → a provider accepted it
--   FAILED                                → terminal; no further attempt

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS delivery_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMP WITH TIME ZONE;

DO $$
BEGIN
    -- A concluded notification must not be scheduled for another attempt.
    -- Without this, a bug that forgot to clear next_attempt_at on success
    -- would have the worker re-sending a message that was already delivered —
    -- the duplicate-notice failure ZS-SVC-Y-001 §0.4 names directly.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'notifications_concluded_has_no_retry'
           AND conrelid = 'notifications'::regclass
    ) THEN
        ALTER TABLE notifications
            ADD CONSTRAINT notifications_concluded_has_no_retry
            CHECK (status = 'PENDING' OR next_attempt_at IS NULL) NOT VALID;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'notifications_attempts_non_negative'
           AND conrelid = 'notifications'::regclass
    ) THEN
        ALTER TABLE notifications
            ADD CONSTRAINT notifications_attempts_non_negative
            CHECK (delivery_attempts >= 0) NOT VALID;
    END IF;
END $$;

-- The worker's claim query, and only that. Partial because due retries are a
-- vanishingly small slice of a delivery register that only grows — an index
-- over every notification would be almost entirely rows that can never match.
CREATE INDEX IF NOT EXISTS idx_notifications_due_retry
    ON notifications (next_attempt_at)
    WHERE next_attempt_at IS NOT NULL AND status = 'PENDING';

-- ── The cross-tenant problem, and why this is the narrowest answer ──────────
--
-- The retry worker is not serving a request. Nobody's tenant is installed on
-- its connection, and notifications is FORCE ROW LEVEL SECURITY with a policy
-- keyed on app.tenant_id — so the worker, correctly, can see nothing at all.
-- It cannot even discover WHICH tenants have work waiting.
--
-- Three options were available:
--
--   1. A separate un-scoped retry-schedule table. Rejected: it duplicates
--      state that must never disagree with the notification it describes, and
--      keeping two rows in step is exactly the kind of sync nobody revisits.
--   2. A SECURITY DEFINER function. Rejected: under FORCE RLS it only works if
--      owned by a BYPASSRLS role, which is a bigger hole than this one and an
--      invisible one.
--   3. This: the platform-scope hatch the platform already uses — 0028
--      documents the same mechanism on authorization_svc.roles.
--
-- It is deliberately the narrowest form of that hatch:
--
--   FOR SELECT ONLY. A platform-scoped connection can DISCOVER work. It cannot
--   insert, update or delete across tenants — every write still requires the
--   correct app.tenant_id, so the worker's actual retry runs tenant-scoped
--   like any request would.
--
-- RLS cannot restrict columns, so this policy does expose message bodies to a
-- connection that sets the flag. What bounds that is the caller: the worker's
-- claim query projects notification_id and tenant_id and nothing else, then
-- drops platform scope and re-enters per tenant to read the message itself.
-- No message content crosses the hatch.
--
-- set_config(..., true) makes the flag transaction-local, so it cannot survive
-- on a pooled connection into somebody's request.
--
-- No TO clause, matching every other policy on this table and matching the
-- Supabase counterpart (0033) for the same reason.
--
-- A policy names the roles it applies to, and a policy that names a role the
-- service does not connect as matches nobody: with FORCE ROW LEVEL SECURITY
-- that means zero rows, so the worker would find no due retries, ever, without
-- an error anywhere. Supabase hit precisely this — its 0026 had to drop
-- `TO zoiko_backend` platform-wide because services connect as a per-service
-- role that is deliberately NOT a member of it.
--
-- Isolation here does not rest on the TO clause in any case: the store carries
-- an explicit `tenant_id = $n` predicate on every statement, and on Supabase
-- the per-service grants confine a role to its own schema.
DROP POLICY IF EXISTS platform_scope_read_policy ON notifications;
CREATE POLICY platform_scope_read_policy ON notifications
    FOR SELECT
    USING (current_setting('app.platform_scope', true) = 'true');
