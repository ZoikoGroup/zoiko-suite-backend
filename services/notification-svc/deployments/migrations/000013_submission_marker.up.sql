-- A durable "submitting" marker, so a lost attempt can be told from one that
-- never started (ZS-SVC-Y-001 §3.4, §6.2).
--
-- §6.2: "UNKNOWN never authorizes an immediate blind second material send."
-- §3.4: "A timeout or network break after submit becomes UNKNOWN, not FAILED.
-- NCD queries or reconciles the original provider attempt before creating
-- another material attempt."
--
-- THE HAZARD THIS CLOSES. A notification left PENDING with nothing scheduled
-- (the process died mid-attempt, or the outcome write failed) is reclaimed by
-- the stranded sweep, which re-schedules it — and its own comment said,
-- correctly, that "a stranded row may or may not have reached the provider
-- before its attempt died, and nothing on the row can distinguish those."
-- Re-scheduling a row that DID reach the provider sends the notice twice: for
-- a payslip or a legal notice, exactly the duplicate §3.4 exists to prevent.
--
-- submitting_since is what makes the two distinguishable. It is set, in its
-- own committed transaction, immediately BEFORE a provider is called, and
-- cleared by whichever transition records the attempt's effect
-- (CompleteDelivery, ScheduleRetry, MarkOutcomeUnknown). So a stranded row:
--
--   * with submitting_since NULL never reached a provider — reviving it cannot
--     duplicate anything, and the sweep does so;
--   * with submitting_since set was handed to a provider and its outcome was
--     lost — it becomes PENDING_UNKNOWN for a person to resolve against the
--     provider's records, instead of being blindly re-sent.
--
-- IN_APP never sets it: the register row IS the delivery, so an in-app attempt
-- cannot be ambiguous.

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS submitting_since TIMESTAMPTZ;

-- A concluded or ambiguous notification is not being submitted. NOT VALID so
-- the constraint applies to new writes without scanning history (every
-- existing row has NULL, which satisfies it anyway).
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_submitting_only_pending;
ALTER TABLE notifications ADD CONSTRAINT notifications_submitting_only_pending
    CHECK (status = 'PENDING' OR submitting_since IS NULL) NOT VALID;
