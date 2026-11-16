-- Migration: 000012_add_eventing_outbox.up.sql
--
-- Moves reconciliation-engine-svc onto the shared eventing outbox
-- (services/eventing, ZS-EVENT-001 §6), retiring the service's own
-- outbox_events table (000001/000002) and its unleased, unbounded relay.
--
-- Part 1 is outbox.SchemaSQL VERBATIM and Part 2 is outbox.HardeningSQL
-- VERBATIM (the library: "new adopters append it after SchemaSQL").
-- internal/store's TestEventingOutboxMigrationMatchesLibrary fails if either
-- drifts. Do not edit them here; change the library and copy them again.

-- ── Part 1: outbox.SchemaSQL ──
CREATE TABLE eventing_outbox (
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

-- Everything not yet delivered, oldest first. Serves the dispatcher's claim
-- scan and the backlog signals; published rows (which accumulate until
-- retention removes them) are outside it, so neither query slows as history
-- grows.
CREATE INDEX eventing_outbox_backlog_idx ON eventing_outbox (created_at)
    WHERE publish_state IN ('pending', 'failed', 'claimed');
CREATE INDEX eventing_outbox_quarantined_idx ON eventing_outbox (created_at)
    WHERE publish_state = 'quarantined';

-- ── Part 2: outbox.HardeningSQL ──
CREATE FUNCTION eventing_outbox_payload_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.event_id IS DISTINCT FROM OLD.event_id
       OR NEW.event_type IS DISTINCT FROM OLD.event_type
       OR NEW.schema_version IS DISTINCT FROM OLD.schema_version
       OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.region IS DISTINCT FROM OLD.region
       OR NEW.partition_key IS DISTINCT FROM OLD.partition_key
       OR NEW.payload IS DISTINCT FROM OLD.payload
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'eventing_outbox event content is immutable'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER eventing_outbox_content_immutable
    BEFORE UPDATE ON eventing_outbox
    FOR EACH ROW EXECUTE FUNCTION eventing_outbox_payload_immutable();

-- ── Part 3: tenant isolation ──
-- The same policy 000002 put on outbox_events, moved to the new table: a
-- request transaction (withTenant: app.tenant_id) sees and writes only its
-- own tenant's events; the shared relay's transactions carry
-- app.outbox_relay = 'true' (outbox.inRelayTx) and see every tenant's.
-- NULLIF because a transaction-local custom GUC reads as '' (not NULL) on a
-- pooled connection that has already served a request.
ALTER TABLE eventing_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY eventing_outbox_tenant_isolation ON eventing_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

-- ── Part 4: carry over every event the old relay had not yet delivered ──
-- outbox_events.payload holds the bare business object; the old publisher
-- (internal/events) wrapped it at publish time. Each carried-over row is that
-- same wrapper, built here from the row itself, so consumers receive the
-- body shape they already parse:
--   * event_id is 'evt-' || outbox_event_id: the old publisher's "evt-<uuid>"
--     form, but stable. The old one minted a new id on every attempt and sent
--     no X-Event-ID header, so no consumer can hold an id for these rows.
--   * occurred_at is the row's created_at, written in the same transaction as
--     the fact (the old publisher used its own publish-time clock).
--   * legal_entity_id, actor_id and correlation_id stay absent, as they were
--     on every relayed event: outbox_events never recorded them.
--   * tenant_id is the row's own. A row without one fails NOT NULL here
--     rather than being given a tenant it never had.
--   * region is the literal 'legacy': these rows predate residency stamping
--     and nothing records which region produced them. It marks them; it is
--     not a region code.
--   * the partition key uses the aggregate type the service now emits for
--     that aggregate, so a run's carried-over and new events share a
--     partition. An aggregate type not listed fails NOT NULL (sha256 of NULL).
--   * every row arrives pending: the old relay had no attempt cap, so none
--     was ever dead-lettered.
-- outbox_events itself is left untouched: its rows are the delivery evidence
-- for everything published before this migration (ZS-EVENT-001 §6.1: cleanup
-- "must not delete evidence required for reconciliation before delivery is
-- proven"). Nothing writes to it any more.
--
-- During a rolling deploy an old pod's relay can still publish a carried-over
-- row the new relay also publishes, under a different event id (the old one
-- mints its own); the legacy body's subject_id and event_type identify the
-- duplicate (at-least-once, §7).
INSERT INTO eventing_outbox (
    outbox_id, event_id, event_type, schema_version, tenant_id, region,
    aggregate_id, aggregate_version, partition_key, payload, payload_hash,
    created_at, publish_state, attempt_count, last_error
)
SELECT
    o.outbox_event_id,
    'evt-' || o.outbox_event_id::text,
    o.event_type,
    '1.0.0',
    o.tenant_id,
    'legacy',
    o.aggregate_id,
    NULL,
    -- Same formula as envelope.PartitionKey: sha256(tenant 0x00 type 0x00 id).
    encode(sha256(convert_to(o.tenant_id, 'UTF8') || '\x00'::bytea
                  || convert_to(a.aggregate, 'UTF8') || '\x00'::bytea
                  || convert_to(o.aggregate_id, 'UTF8')), 'hex'),
    convert_to(jsonb_build_object(
        'event_id',       'evt-' || o.outbox_event_id::text,
        'event_type',     o.event_type,
        'event_version',  '1.0',
        'source_service', 'reconciliation-engine-svc',
        'subject_id',     o.aggregate_id,
        'tenant_id',      o.tenant_id,
        'occurred_at',    to_char(o.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
        'payload',        o.payload
    )::text, 'UTF8'),
    'sha256:' || encode(sha256(convert_to(o.payload::text, 'UTF8')), 'hex'),
    o.created_at,
    'pending',
    0,
    o.last_error
FROM outbox_events o
LEFT JOIN (VALUES
    ('reconciliation_run',        'reconciliation-run'),
    ('reconciliation_exception',  'reconciliation-break'),
    ('certification',             'reconciliation-certification'),
    ('reconciliation_definition', 'reconciliation-definition')
) AS a(legacy, aggregate) ON a.legacy = o.aggregate_type
WHERE o.published_at IS NULL;
