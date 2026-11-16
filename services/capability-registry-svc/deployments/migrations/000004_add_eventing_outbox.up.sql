-- Migration: 000004_add_eventing_outbox.up.sql
--
-- Moves capability-registry-svc onto the shared eventing outbox
-- (services/eventing, ZS-EVENT-001 §6), retiring the service's own
-- outbox_events table (000003) and its unleased, unbounded relay.
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

-- Part 3: platform scope, enforced in the row itself.
--
-- Every object this service owns is platform catalogue data with no tenant
-- (SPEC_DEVIATIONS B12), so every event carries the one platform scope id
-- (domain.PlatformScopeID) as tenant_id — SPEC_DEVIATIONS B13. The sibling
-- adopters' tenant-isolation RLS policy is deliberately NOT applied: with a
-- single legitimate tenant_id value it can never separate one tenant's rows
-- from another's, yet under FORCE RLS it would cost a set_config round-trip
-- on every write and hard-fail any write that omitted it. What is worth
-- protecting is that no code path ever stamps a different tenant_id; this
-- CHECK enforces exactly that, in the INSERT itself, with no session state.
ALTER TABLE eventing_outbox
    ADD CONSTRAINT eventing_outbox_platform_scope
    CHECK (tenant_id = '00000000-0000-0000-0000-00000000f001');

-- Part 4: carry over every event the old relay had not yet delivered from
-- outbox_events (000003), so retiring that relay loses nothing — the same
-- approach as general-ledger-svc's 000013, adjusted to this service's rows:
--   * event_id is the payload's own event_id ("evt-<uuid>"). That, not
--     outbox_event_id, is what the old publisher sent as the X-Event-ID
--     header, so a consumer that saw an earlier attempt dedupes the
--     redelivery. The old publisher refused any payload without one, so the
--     COALESCE fallback only covers a row that could never have been sent.
--   * payload is the old event body, unchanged; the new relay only adds
--     publishedat to it.
--   * tenant_id is the platform scope (Part 3) — what these events always
--     described; the old body omitted tenant_id for the same reason.
--   * region is the literal 'legacy': these rows predate residency stamping
--     and nothing records which region produced them. It marks them; it is
--     not a region code.
--   * partition_key uses envelope.PartitionKey's formula over the old Kafka
--     key (entity_id). For the five event types keyed on a capability_id
--     this is exactly the key new events for that capability get, so a
--     carried-over row and later events for the same capability stay on one
--     partition and in order. integration_capability.health_updated was keyed
--     on the integration_capability_id and keeps that key.
--   * aggregate_id is NULL: the old body has no subject, and
--     Relay.VerifyPartitioning groups aggregates by subject, so setting it
--     would report every carried-over row as one aggregate with mixed keys.
--   * every row arrives pending with a fresh retry budget: the old relay had
--     no attempt limit and never dead-lettered anything, so there is no
--     prior give-up decision to carry over.
-- outbox_events itself is left in place: its rows are the delivery evidence
-- for everything published before this migration (ZS-EVENT-001 §6.1: cleanup
-- "must not delete evidence required for reconciliation before delivery is
-- proven"). Nothing writes to it any more.
--
-- During a rolling deploy an old pod's relay can still publish a carried-over
-- row the new relay also publishes. Both carry the same event id; consumers
-- dedupe (at-least-once, §7).
INSERT INTO eventing_outbox (
    outbox_id, event_id, event_type, schema_version, tenant_id, region,
    aggregate_id, aggregate_version, partition_key, payload, payload_hash,
    created_at, publish_state, attempt_count, last_error
)
SELECT
    o.outbox_event_id,
    COALESCE(NULLIF(o.payload ->> 'event_id', ''), o.outbox_event_id::text),
    -- No fallback: every old row was built by events.NewEvent, which always
    -- set event_type. A row without one fails the NOT NULL loudly rather
    -- than being relabelled with an invented type.
    o.payload ->> 'event_type',
    '1.0.0',
    '00000000-0000-0000-0000-00000000f001',
    'legacy',
    NULL,
    NULL,
    -- Same formula as envelope.PartitionKey: sha256(tenant 0x00 type 0x00 id).
    encode(sha256(convert_to('00000000-0000-0000-0000-00000000f001', 'UTF8') || '\x00'::bytea
                  || convert_to(CASE WHEN o.payload ->> 'event_type' = 'integration_capability.health_updated'
                                     THEN 'integration-capability' ELSE 'capability' END, 'UTF8')
                  || '\x00'::bytea
                  || convert_to(o.entity_id, 'UTF8')), 'hex'),
    convert_to(o.payload::text, 'UTF8'),
    'sha256:' || encode(sha256(convert_to(COALESCE(o.payload -> 'payload', o.payload)::text, 'UTF8')), 'hex'),
    o.created_at,
    'pending',
    0,
    o.last_error
FROM outbox_events o
WHERE o.published_at IS NULL;
