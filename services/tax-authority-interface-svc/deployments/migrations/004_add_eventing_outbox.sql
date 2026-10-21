-- Transactional event outbox (ZS-EVENT-001 section 6). The authority adapter emits its material events (attempt
-- submitted, unknown outcome, response received, deadline at risk, profile changed) in the same transaction as
-- the state change they report. Verbatim from services/eventing/outbox/schema.go:SchemaSQL, which
-- VerifySchema() checks at startup. Re-runnable.

CREATE TABLE IF NOT EXISTS eventing_outbox (
    outbox_id           UUID         PRIMARY KEY,
    event_id            TEXT         NOT NULL UNIQUE,
    event_type          TEXT         NOT NULL,
    schema_version      TEXT         NOT NULL,
    tenant_id           TEXT         NOT NULL,
    region              TEXT         NOT NULL,
    aggregate_id        TEXT,
    aggregate_version   BIGINT,
    partition_key       TEXT         NOT NULL,
    payload             BYTEA        NOT NULL,
    payload_hash        TEXT         NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    publish_state       TEXT         NOT NULL DEFAULT 'pending',
    attempt_count       INTEGER      NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    claimed_until       TIMESTAMPTZ,
    dispatched_at       TIMESTAMPTZ,
    published_at        TIMESTAMPTZ,
    last_error_code     TEXT,
    last_error          TEXT,

    CONSTRAINT eventing_outbox_state_valid CHECK (publish_state IN
        ('pending', 'claimed', 'published', 'failed', 'quarantined')),
    CONSTRAINT eventing_outbox_attempts_nonneg CHECK (attempt_count >= 0),
    CONSTRAINT eventing_outbox_published_has_time CHECK
        (publish_state <> 'published' OR published_at IS NOT NULL),
    CONSTRAINT eventing_outbox_claimed_has_lease CHECK
        (publish_state <> 'claimed' OR claimed_until IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS eventing_outbox_backlog_idx ON eventing_outbox (created_at)
    WHERE publish_state IN ('pending', 'failed', 'claimed');
CREATE INDEX IF NOT EXISTS eventing_outbox_quarantined_idx ON eventing_outbox (created_at)
    WHERE publish_state = 'quarantined';

ALTER TABLE eventing_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS eventing_outbox_tenant_isolation ON eventing_outbox;
CREATE POLICY eventing_outbox_tenant_isolation ON eventing_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
