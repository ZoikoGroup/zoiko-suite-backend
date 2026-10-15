-- Transactional outbox. Events are inserted by the same transaction that
-- changes state; internal/outbox drains them to Kafka
-- (zoiko.fiscal-calendar.events). The payload is the fully built envelope,
-- built at write time so it carries the actor and correlation of the request
-- that caused the change.
CREATE TABLE IF NOT EXISTS fiscal_calendar_outbox (
    outbox_id     BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    object_id     UUID NOT NULL,            -- calendar_id, or version_id for version events
    event_type    TEXT NOT NULL,
    payload       JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at  TIMESTAMPTZ,
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    CONSTRAINT fiscal_calendar_outbox_event_known CHECK (event_type IN
        ('FiscalCalendarCreated', 'FiscalCalendarChangeProposed', 'FiscalCalendarVersionActivated', 'FiscalCalendarSuperseded'))
);

-- The relay's claim query: unpublished rows, oldest first. Partial, so the
-- index stays the size of the backlog rather than the history.
CREATE INDEX IF NOT EXISTS idx_fiscal_calendar_outbox_unpublished
    ON fiscal_calendar_outbox (created_at, outbox_id) WHERE published_at IS NULL;

ALTER TABLE fiscal_calendar_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE fiscal_calendar_outbox FORCE ROW LEVEL SECURITY;

-- Request-path writes are confined to the actor's tenant. The relay is not a
-- request: it installs app.outbox_relay and is admitted by the second disjunct.
-- NULLIF because a pooled connection keeps a custom GUC as '' after the
-- transaction-local SET resets.
CREATE POLICY outbox_tenant_isolation ON fiscal_calendar_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
