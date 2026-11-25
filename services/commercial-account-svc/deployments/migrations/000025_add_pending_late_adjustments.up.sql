-- 000025_add_pending_late_adjustments.up.sql
-- COM-04 Usage Metering, part 4 (ZS-SVC-Q-001 §4.4, §9 negative path #18;
-- SPEC_DEVIATIONS.md A8): a durable late-event inbox so registerLateEvent
-- never loses a late usage fact when no target statement exists yet.
--
-- Today registerLateEvent needs a target OPEN statement to exist (or to be
-- creatable) in the SAME transaction that records the late event, in two
-- real cases it cannot provide one:
--   - the origin term's own statement is FROZEN (not yet certified) and
--     nothing later is open yet (findOrCreateOpenStatement ->
--     ErrStatementInvalidState);
--   - the subscription has ended and there is no term covering "now"
--     (resolveTerm -> ErrNoOpenTermForCorrection).
-- Today both cases roll back the whole transaction, so even the late
-- usage_event_records row is lost -- strictly worse than "Late events after
-- certified cutoff create adjustment, not mutation" (§4.4).
--
-- pending_late_adjustments is the structural fix the deviation log itself
-- names: the late fact's full data is made durable WITHOUT requiring a
-- target to exist yet. A worker, shaped exactly like this service's other
-- two durable queues (subscription_boundary_queue, migration 000009;
-- entitlement_invalidation_queue, migration 000024 -- see com03's own
-- comment on reusing ProcessNextBoundary's shape), re-resolves a target at
-- its OWN "now" on every pass and attaches the adjustment the moment one
-- becomes available: a new term opening (TERM_END -> renewal) is what makes
-- a same-term-frozen case resolvable, since nowTermNo at RETRY time (not the
-- original observedAt) lands in whichever term is current when the worker
-- runs. There is deliberately no terminal FAILED/expiry state and no max
-- attempts: the spec gives no expiry for a late correction, so an item with
-- no resolvable target yet (including a subscription that has permanently
-- ended with no further term) stays PENDING and keeps being retried with a
-- capped backoff, rather than being discarded or silently dropped.
CREATE TABLE pending_late_adjustments (
    pending_id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id         UUID         NOT NULL,
    subscription_id         TEXT         NOT NULL REFERENCES subscriptions (subscription_id),
    meter_key               VARCHAR(128) NOT NULL,
    meter_version           INT          NOT NULL,
    origin_statement_id     TEXT         NOT NULL REFERENCES usage_statements (statement_id),
    origin_term_no          INT          NOT NULL,
    source_usage_event_id   TEXT         NOT NULL,
    quantity                NUMERIC      NOT NULL CHECK (scale(quantity) <= 4),
    occurred_at             TIMESTAMPTZ  NOT NULL,
    dimensions              JSONB        NOT NULL DEFAULT '{}',
    reason                  TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    created_at              TIMESTAMPTZ  NOT NULL,
    attempts                INT          NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error              TEXT,
    next_attempt_at         TIMESTAMPTZ  NOT NULL,
    status                  VARCHAR(16)  NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'ATTACHED')),
    attached_at             TIMESTAMPTZ,
    attached_adjustment_id  TEXT         REFERENCES usage_adjustments (adjustment_id),
    FOREIGN KEY (meter_key, meter_version) REFERENCES meter_definitions (meter_key, meter_version),
    FOREIGN KEY (meter_key, source_usage_event_id) REFERENCES usage_event_records (meter_key, usage_event_id),
    CONSTRAINT pending_late_adjustments_attached_evidence CHECK (
        (status = 'PENDING') = (attached_at IS NULL)
        AND (status = 'PENDING') = (attached_adjustment_id IS NULL)),
    -- One pending row per late event: registerLateEvent's own dedup
    -- (usage_event_records' (meter_key, usage_event_id) primary key) already
    -- stops a replay from recording the event twice; this stops it from
    -- queuing a second pending adjustment for the same already-queued one.
    CONSTRAINT pending_late_adjustments_one_per_event UNIQUE (meter_key, source_usage_event_id)
);

CREATE INDEX idx_pending_late_adjustments_due ON pending_late_adjustments (next_attempt_at) WHERE status = 'PENDING';

-- Append-only except for the one transition a worker makes: PENDING ->
-- ATTACHED (plus its evidence columns), or recording a failed attempt
-- (attempts/last_error/next_attempt_at) while it stays PENDING.
CREATE FUNCTION enforce_pending_late_adjustment_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    retry_cols  TEXT[] := ARRAY['attempts', 'last_error', 'next_attempt_at'];
    attach_cols TEXT[] := ARRAY['attempts', 'last_error', 'next_attempt_at', 'status', 'attached_at', 'attached_adjustment_id'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'pending late adjustment % cannot be deleted', OLD.pending_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status = 'ATTACHED' THEN
        RAISE EXCEPTION 'pending late adjustment % is already attached and final', OLD.pending_id USING ERRCODE = 'CP001';
    END IF;
    IF NEW.status = 'ATTACHED' AND (to_jsonb(NEW) - attach_cols) IS NOT DISTINCT FROM (to_jsonb(OLD) - attach_cols) THEN
        RETURN NEW;
    END IF;
    IF NEW.status = 'PENDING' AND (to_jsonb(NEW) - retry_cols) IS NOT DISTINCT FROM (to_jsonb(OLD) - retry_cols) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'pending late adjustment %: the only changes are recording a retry or moving PENDING -> ATTACHED',
        OLD.pending_id USING ERRCODE = 'CP001';
END;
$$;

CREATE TRIGGER trg_pending_late_adjustments_lifecycle
    BEFORE UPDATE OR DELETE ON pending_late_adjustments
    FOR EACH ROW EXECUTE FUNCTION enforce_pending_late_adjustment_lifecycle();

-- RLS: same posture as usage_statements/usage_event_records/usage_adjustments
-- (migration 000011) -- ingestion and worker attachment are granted
-- workloads acting on a named organization, never the tenant acting on
-- itself, but the owning organization can still read its own pending
-- late-adjustment evidence.
ALTER TABLE pending_late_adjustments ENABLE ROW LEVEL SECURITY;
ALTER TABLE pending_late_adjustments FORCE ROW LEVEL SECURITY;
CREATE POLICY pending_late_adjustments_access ON pending_late_adjustments FOR ALL
    USING (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker')
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker'));
