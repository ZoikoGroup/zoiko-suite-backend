-- Migration: 000012_add_eventing_outbox.up.sql
--
-- Moves financial-control-svc onto the shared eventing outbox
-- (services/eventing, ZS-EVENT-001 §6), retiring the service's own
-- outbox_events table (000001) and its relay and publisher.
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
-- 000001 left outbox_events without RLS because its relay polled every
-- tenant with no tenant scope set. The shared relay marks its transactions
-- with app.outbox_relay = 'true' (outbox.inRelayTx), so this table can carry
-- the same isolation as every other tenant table: a request transaction
-- (withRLS sets app.tenant_id) sees and writes only its own tenant's events,
-- and the relay sees every tenant's. SPEC_DEVIATIONS B10.
--
-- Only the SETTING is cast to UUID, as in 000001's policies, so any spelling
-- of the tenant UUID the request supplied (upper case, braces) still matches.
-- The stored tenant_id is always canonical text: it comes from a UUID column
-- (scanRun) or, in Part 4, from tenant_id::text. ::uuid::text puts the
-- setting in that same form. The column itself is never cast, so no row can
-- make a policy check, or the relay's scan, raise an error.
-- NULLIF because a transaction-local custom GUC reads as '' (not NULL) on a
-- pooled connection that has already served a request.
ALTER TABLE eventing_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY eventing_outbox_tenant_isolation ON eventing_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid::text
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid::text
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );

-- ── Part 4: carry over every event the old relay had not yet delivered ──
-- outbox_events.payload holds the bare event data; the old publisher
-- (internal/events) wrapped it at publish time. Each carried-over row is that
-- wrapper, built here from the row itself, so consumers receive the body
-- shape they already parse:
--   * event_id is the old outbox_event_id: the value the old publisher sent as
--     event_id and as the X-Event-ID header on every attempt, so a consumer
--     that saw an earlier attempt dedupes the redelivery.
--   * tenant_id and legal_entity_id are UUID columns in outbox_events and text
--     in eventing_outbox and the body. Both are cast explicitly (::text, the
--     canonical lower-case form new events carry), never implicitly.
--   * emitted_at is the row's created_at, as the old publisher sent it.
--     aggregate_version is the payload's "version" where that is a number, as
--     the old publisher derived it.
--   * published_at, residency_region and classification are left out of the
--     body. The old publisher stamped them at publish time from its own clock
--     and configuration, which this migration cannot know. The new relay adds
--     publishedat on delivery.
--   * region is the literal 'legacy': these rows predate residency stamping
--     here. It marks them; it is not a region code.
--   * the partition key uses the aggregate type the service now emits, so a
--     run's carried-over and new events share a partition. An aggregate type
--     not listed fails NOT NULL (sha256 of NULL) instead of being keyed on a
--     guess, and the whole migration rolls back.
--   * rows the old relay had dead-lettered (publish_attempts >= 10, its
--     MaxPublishAttempts, after which it stopped polling them) arrive
--     quarantined, not pending. They need an owner's decision
--     (outbox.Relay.Requeue), not an automatic eleventh try.
-- outbox_events itself is left untouched: its rows are the delivery evidence
-- for everything published before this migration (ZS-EVENT-001 §6.1: cleanup
-- "must not delete evidence required for reconciliation before delivery is
-- proven"). Nothing writes to it any more.
--
-- During a rolling deploy an old pod's relay can still publish a carried-over
-- row the new relay also publishes. Both carry the same event id, so
-- consumers dedupe (at-least-once, §7).
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
    v.version,
    -- Same formula as envelope.PartitionKey: sha256(tenant 0x00 type 0x00 id).
    encode(sha256(convert_to(o.tenant_id::text, 'UTF8') || '\x00'::bytea
                  || convert_to(a.aggregate, 'UTF8') || '\x00'::bytea
                  || convert_to(o.aggregate_id, 'UTF8')), 'hex'),
    convert_to((
        jsonb_build_object(
            'event_type',      o.event_type,
            'event_version',   '1.0',
            'emitted_at',      to_char(o.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
            'schema_version',  '1.0',
            'source_service',  'financial-control-svc',
            'correlation_id',  COALESCE(o.correlation_id, ''),
            'tenant_id',       o.tenant_id::text,
            'legal_entity_id', o.legal_entity_id::text,
            'payload',         o.payload,
            'event_id',        o.outbox_event_id::text,
            'aggregate_type',  o.aggregate_type,
            'aggregate_id',    o.aggregate_id,
            'payload_hash',    'sha256:' || encode(sha256(convert_to(o.payload::text, 'UTF8')), 'hex')
        )
        || CASE WHEN NULLIF(o.actor_id, '') IS NOT NULL
                THEN jsonb_build_object('actor_id', o.actor_id) ELSE '{}'::jsonb END
        || CASE WHEN v.version IS NOT NULL
                THEN jsonb_build_object('aggregate_version', v.version) ELSE '{}'::jsonb END
    )::text, 'UTF8'),
    'sha256:' || encode(sha256(convert_to(o.payload::text, 'UTF8')), 'hex'),
    o.created_at,
    CASE WHEN o.publish_attempts >= 10 THEN 'quarantined' ELSE 'pending' END,
    CASE WHEN o.publish_attempts >= 10 THEN o.publish_attempts ELSE 0 END,
    CASE WHEN o.publish_attempts >= 10 THEN 'MIGRATED_DEAD_LETTER' END,
    o.last_error
FROM outbox_events o
LEFT JOIN (VALUES
    ('control_run',       'control-run'),
    ('control_exception', 'control-exception')
) AS a(legacy, aggregate) ON a.legacy = o.aggregate_type
CROSS JOIN LATERAL (
    SELECT CASE WHEN jsonb_typeof(o.payload -> 'version') = 'number'
                THEN (o.payload ->> 'version')::bigint END AS version
) AS v
WHERE o.published_at IS NULL;
