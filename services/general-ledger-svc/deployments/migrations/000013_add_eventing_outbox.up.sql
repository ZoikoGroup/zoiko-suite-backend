-- Migration: 000013_add_eventing_outbox.up.sql
--
-- Moves general-ledger-svc onto the shared eventing outbox
-- (services/eventing, ZS-EVENT-001 §6).
--
-- Part 1 is outbox.SchemaSQL VERBATIM — internal/store's
-- TestEventingOutboxMigrationMatchesLibrary fails if the two drift. Do not
-- edit it here; change the library and copy it again.
--
-- Part 2 carries over every event the old relay had not yet delivered from
-- outbox_events (000012), so retiring that relay loses nothing:
--   * event_id is the old outbox_event_id — the value the old relay sent as
--     the X-Event-ID header, so a consumer that saw an earlier attempt
--     dedupes the redelivery.
--   * payload is the old Variant A envelope, unchanged. It predates the
--     canonical envelope (no id, residencyregion or payloadhash in the body);
--     the new relay only adds publishedat to it.
--   * region is the literal 'legacy': these rows predate residency stamping
--     and nothing records which region produced them. It marks them; it is
--     not a region code.
--   * rows the old relay had dead-lettered (publish_attempts >= 10, its
--     MaxPublishAttempts) arrive quarantined, not pending — they failed ten
--     times and need an owner's decision (outbox.Relay.Requeue), not an
--     automatic eleventh try.
-- outbox_events itself is left untouched: its rows are the delivery evidence
-- for everything published before this migration (ZS-EVENT-001 §6.1: cleanup
-- "must not delete evidence required for reconciliation before delivery is
-- proven"). Nothing writes to it any more.
--
-- During a rolling deploy an old pod's relay can still publish a carried-over
-- row the new relay also publishes. Both carry the same event id; consumers
-- dedupe (at-least-once, §7).

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

INSERT INTO eventing_outbox (
    outbox_id, event_id, event_type, schema_version, tenant_id, region,
    aggregate_id, aggregate_version, partition_key, payload, payload_hash,
    created_at, publish_state, attempt_count, last_error_code, last_error
)
SELECT
    o.outbox_event_id,
    o.outbox_event_id::text,
    o.event_type,
    '1.0.0',
    o.tenant_id::text,
    'legacy',
    o.aggregate_id,
    NULL,
    -- Same formula as envelope.PartitionKey: sha256(tenant 0x00 type 0x00 id).
    encode(sha256(convert_to(o.tenant_id::text, 'UTF8') || '\x00'::bytea
                  || convert_to(lower(o.aggregate_type), 'UTF8') || '\x00'::bytea
                  || convert_to(o.aggregate_id, 'UTF8')), 'hex'),
    convert_to(o.payload::text, 'UTF8'),
    'sha256:' || encode(sha256(convert_to(COALESCE(o.payload -> 'payload', o.payload)::text, 'UTF8')), 'hex'),
    o.created_at,
    CASE WHEN o.publish_attempts >= 10 THEN 'quarantined' ELSE 'pending' END,
    CASE WHEN o.publish_attempts >= 10 THEN o.publish_attempts ELSE 0 END,
    CASE WHEN o.publish_attempts >= 10 THEN 'MIGRATED_DEAD_LETTER' END,
    o.last_error
FROM outbox_events o
WHERE o.published_at IS NULL;
