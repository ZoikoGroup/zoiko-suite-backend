-- A transactional outbox for the two notification events.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- WHY
--
-- Until now notification.sent and notification.failed were written straight to
-- Kafka, from the handler and from the retry worker, AFTER the store
-- transaction that concluded the delivery had already committed, with the
-- error logged and discarded:
--
--     if err := h.store.CompleteDelivery(...); err != nil { ... }
--     ...
--     h.publisher.PublishSent(outcomeCtx, correlationID, *notification)
--     writeJSON(w, 201, notification)
--
-- Publisher.emit's only answer to a broker refusal was
--
--     p.log.Error("failed to publish event", ...)
--
-- and then it returned. So a broker hiccup at the moment a notice concluded
-- left: the delivery correctly recorded, the caller correctly told 201, the
-- register correctly showing SENT — and no consumer anywhere ever learning
-- that the notification went out, or that it did not.
--
-- WHAT MAKES THAT PARTICULARLY BAD HERE. This service exists to be the evidence
-- that a governed notice was issued. §9.7 gives it "workflows, deadlines,
-- escalations, approvals, and status changes", so an escalation chain waiting
-- on notification.failed before paging a human simply never fires: nothing is
-- in an error state, no retry is scheduled, no metric moves, and the register
-- shows a healthy SENT row. The loss is silent by construction — the one thing
-- that would have reported it is the event that was lost.
--
-- It is also strictly worse than the equivalent on configuration-feature-flag-
-- svc (its 000003, which this mirrors). There, a lost config.updated leaves a
-- consumer serving a superseded but valid value. Here, a lost
-- notification.failed leaves a person who was never told something they were
-- entitled to be told, and a platform that believes they were.
--
-- Writing the event in the SAME transaction as the status transition makes the
-- two atomic: a notification that concluded always has its event, and an event
-- that exists always has its conclusion. Delivery to the broker then becomes a
-- separate, retryable problem that internal/outbox owns and that the gauges in
-- internal/telemetry make visible.
--
-- WHY ONLY THE CONCLUSION IS ENQUEUED. Creating a notification emits nothing,
-- and a scheduled retry emits nothing — deliberately, and this table does not
-- change that. A notification awaiting another attempt has not failed, and
-- publishing a failure that a later attempt reverses would have consumers act
-- on an outcome that did not happen; internal/retry/worker.go and the handler
-- both say so at their call sites. Exactly one event is emitted per
-- notification, when it reaches SENT or FAILED — which is the transition
-- CompleteDelivery performs, and the only place this table is written.

CREATE TABLE IF NOT EXISTS event_outbox (
    outbox_id       BIGSERIAL PRIMARY KEY,

    -- VARCHAR(255) to match notifications.tenant_id exactly, rather than UUID.
    -- Every tenant on this service is a UUID in practice, but the column this
    -- one shadows is not typed that way, and a stricter outbox would reject an
    -- event for a notification the register itself accepted — failing a
    -- delivery write for a reason that has nothing to do with the delivery.
    tenant_id       VARCHAR(255) NOT NULL,

    event_type      VARCHAR(100) NOT NULL,

    -- The Kafka partition key: the notification_id the event is about.
    --
    -- Not the correlation id. Keying on the correlation id is harmless while
    -- exactly one event exists per notification, but it makes the ordering
    -- guarantee depend on that staying true; keying on the aggregate means a
    -- second event about the same notification would land on the same
    -- partition, behind the first.
    aggregate_key   VARCHAR(255) NOT NULL,

    -- The fully-built envelope, byte for byte as it will be written to Kafka.
    -- Built at enqueue time rather than at publish time on purpose: the
    -- envelope carries the actor and the correlation id of the request that
    -- caused the send, and the relay runs long after that request context is
    -- gone.
    payload         JSONB NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,

    -- The only list a deployment can actually violate. An event type this
    -- constraint omits fails the INSERT, and because the enqueue shares the
    -- delivery write's transaction, it fails the WRITE — loudly, at the first
    -- send after the mistake, rather than as a topic nobody is consuming.
    -- Keep in step with internal/events and asyncapi.yaml.
    CONSTRAINT event_outbox_event_known
        CHECK (event_type IN ('notification.sent', 'notification.failed'))
);

-- The relay's claim query, and the only index it needs: unpublished rows,
-- oldest first. Partial, so the index stays the size of the BACKLOG rather than
-- the size of the history — which on this service is the whole delivery record
-- and only ever grows.
CREATE INDEX IF NOT EXISTS idx_event_outbox_unpublished
    ON event_outbox (created_at, outbox_id) WHERE published_at IS NULL;

ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
-- FORCE, for the reason 000002 gives for the notifications table: these
-- services connect as the table owner, and Postgres exempts an owner from
-- row-level security unless FORCE is declared. Without it the policy below
-- would be a control that reads as present and does nothing.
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;

-- Tenant isolation, with one named escape hatch.
--
-- Every enqueue happens inside PgStore.withRLS, which has already installed
-- app.tenant_id for the notification's own tenant, so the ordinary disjunct
-- covers the whole write path. The relay is not a request: it drains every
-- tenant's backlog from one background loop, so it installs app.outbox_relay
-- and is admitted by the second disjunct.
--
-- Naming itself rather than running unscoped is the point. Under FORCE ROW
-- LEVEL SECURITY an unscoped relay would simply match zero rows — presenting as
-- a relay that publishes nothing, reports no error, and leaves a backlog
-- growing behind a healthy-looking process. That is the same failure shape this
-- whole table exists to remove. An explicit, auditable disjunct is admission by
-- a control rather than by the absence of one.
--
-- app.outbox_relay is deliberately a DIFFERENT flag from app.platform_scope,
-- which 000004 grants over `notifications`. That one is SELECT-only and exists
-- so the retry worker can discover work; this one must also UPDATE, to mark a
-- batch published. Reusing the same name would have silently widened the retry
-- hatch from read to write across every tenant's notification bodies.
--
-- NULLIF is not decoration. Postgres keeps a custom GUC in the SESSION after a
-- transaction-local SET is reset, with '' as its value, so on any pooled
-- connection that has already served one request these settings read as the
-- empty string rather than NULL. Without NULLIF a connection between requests
-- would match every outbox row whose tenant_id is '' — and while no row here
-- can carry '', relying on that is relying on a coincidence rather than on a
-- predicate.
--
-- No TO clause, matching every other policy on this database. 000004 sets out
-- at length why: a policy naming a role the service does not connect as matches
-- nobody, and under FORCE that is zero rows with no error anywhere.
--
-- Dropped first so the migration is re-runnable, matching the IF NOT EXISTS on
-- the table. CREATE POLICY has no IF NOT EXISTS, so without this a re-run fails
-- at 42710 and leaves a half-applied migration to unpick by hand.
DROP POLICY IF EXISTS outbox_tenant_isolation ON event_outbox;
CREATE POLICY outbox_tenant_isolation ON event_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
