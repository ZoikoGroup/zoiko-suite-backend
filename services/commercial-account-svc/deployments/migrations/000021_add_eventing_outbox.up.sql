-- Migration: 000021_add_eventing_outbox.up.sql
--
-- Moves commercial-account-svc onto the shared eventing outbox
-- (services/eventing, ZS-EVENT-001 §6), retiring the service's own
-- outbox_events table (000004, the platform's original outbox pilot) and its
-- unleased, unbounded relay.
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

-- ── Part 3: isolation ──
-- 000004 left outbox_events without RLS. eventing_outbox gets the house
-- pattern this service's own tables use (000005, 000010-000015), which has
-- two writers rather than one:
--   * a tenant transaction (app.tenant_id = the organization) sees and writes
--     only its own organization's events;
--   * a seller-plane transaction (app.commercial_plane = 'seller') may write
--     an event for any organization, or for PlatformScopeID: billing,
--     payments, credits, dunning, usage and the catalogue all run there, and
--     the tables those facts live on admit the seller plane in the same way.
--     It cannot read the outbox: nothing on the seller plane needs to;
--   * the shared relay's transactions (app.outbox_relay = 'true',
--     outbox.inRelayTx) see every row.
-- The boundary worker sets both its plane and the organization, so the
-- tenant clause admits it.
--
-- tenant_id is compared as text, like every other policy here (000005
-- explains why: app.tenant_id can be '' for a request with no verified
-- tenant, and ''::uuid would turn that into a 500 instead of no rows). The
-- business tables compare organization_id::text the same way, so a tenant
-- string that would not match here could not have written the business row
-- either. NULLIF because a transaction-local custom GUC reads as '' (not
-- NULL) on a pooled connection that has already served a request.
ALTER TABLE eventing_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE eventing_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY eventing_outbox_access ON eventing_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
        OR current_setting('app.commercial_plane', true) = 'seller'
    );

-- ── Part 4: carry over every event the old relay had not yet delivered ──
-- outbox_events.payload holds the bare event data; the old publisher
-- (internal/events) wrapped it at publish time. Each carried-over row is that
-- wrapper, built here from the row itself, so consumers (audit-event-store-svc
-- subscribes to this topic) receive the body shape they already parse.
--   * event_id is 'evt-' || outbox_event_id: the old publisher's "evt-<uuid>"
--     form, but stable. The old one minted a new id on every attempt, so no
--     consumer can hold one for these rows.
--   * occurred_at is the row's created_at, written in the same transaction as
--     the fact; entity_id is the aggregate id, as the old publisher sent it.
--   * tenant_id is the organization the fact belongs to, never a guess. The
--     old rows record it three ways, and each is resolved from data:
--       - an organization UUID: used as is, cast to text explicitly;
--       - commercial_subscription and billing_source_transfer rows stored the
--         COMMERCIAL ACCOUNT id as tenant (the pilot's bug): mapped to that
--         account's organization_id;
--       - NULL: the catalogue and policy aggregates are platform-scope facts
--         and take PlatformScopeID; usage_statement and usage_adjustment rows
--         were tenant facts written without a tenant (a bug) and take the
--         organization recorded on their subscription's usage events.
--     Anything that resolves to no tenant fails NOT NULL, and the whole
--     migration rolls back.
--   * the partition key uses the aggregate type the service now emits (the
--     old name kebab-cased), so an aggregate's carried-over and new events
--     share a partition. An aggregate type not listed below fails NOT NULL.
--   * region is the literal 'legacy': these rows predate residency stamping.
--     It marks them; it is not a region code.
--   * every row arrives pending: the old relay had no attempt cap.
-- outbox_events itself is left untouched: its rows are the delivery evidence
-- for everything published before this migration (ZS-EVENT-001 §6.1). Nothing
-- writes to it any more.
--
-- During a rolling deploy an old pod's relay can still publish a carried-over
-- row the new relay also publishes, under a different event id (the old one
-- mints its own); entity_id and event_type identify the duplicate.
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
    t.tenant,
    'legacy',
    o.aggregate_id,
    NULL,
    -- Same formula as envelope.PartitionKey: sha256(tenant 0x00 type 0x00 id).
    encode(sha256(convert_to(t.tenant, 'UTF8') || '\x00'::bytea
                  || convert_to(a.aggregate, 'UTF8') || '\x00'::bytea
                  || convert_to(o.aggregate_id, 'UTF8')), 'hex'),
    convert_to(jsonb_build_object(
        'event_id',       'evt-' || o.outbox_event_id::text,
        'event_type',     o.event_type,
        'event_version',  '1.0',
        'schema_version', '1.0',
        'source_service', 'commercial-account-svc',
        'entity_id',      o.aggregate_id,
        'tenant_id',      t.tenant,
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
    -- Organization-scoped (tenant_id is the organization)
    ('commercial_account', 'commercial-account', 'org'),
    ('membership', 'membership', 'org'),
    ('subscription', 'subscription', 'org'),
    ('subscription_discount', 'subscription-discount', 'org'),
    ('commercial_entitlement', 'commercial-entitlement', 'org'),
    ('commercial_restriction', 'commercial-restriction', 'org'),
    ('usage_event', 'usage-event', 'org'),
    ('billing_account', 'billing-account', 'org'),
    ('invoice_candidate', 'invoice-candidate', 'org'),
    ('platform_commercial_invoice', 'platform-commercial-invoice', 'org'),
    ('credit_note', 'credit-note', 'org'),
    ('write_off', 'write-off', 'org'),
    ('refund_request', 'refund-request', 'org'),
    ('dunning_case', 'dunning-case', 'org'),
    ('commercial_reconciliation', 'commercial-reconciliation', 'org'),
    ('accounting_event', 'accounting-event', 'org'),
    ('dispute_case', 'dispute-case', 'org'),
    ('payment_attempt', 'payment-attempt', 'org'),
    -- tenant_id holds the commercial account id (the pilot's bug)
    ('commercial_subscription', 'commercial-subscription', 'account'),
    ('billing_source_transfer', 'billing-source-transfer', 'account'),
    -- Tenant facts written without a tenant (a bug)
    ('usage_statement', 'usage-statement', 'statement'),
    ('usage_adjustment', 'usage-adjustment', 'adjustment'),
    -- Platform-scope facts: the seller's catalogue and policies
    ('plan_transition_rule', 'plan-transition-rule', 'platform'),
    ('price_migration_offer', 'price-migration-offer', 'platform'),
    ('entitlement_policy', 'entitlement-policy', 'platform'),
    ('meter_definition', 'meter-definition', 'platform'),
    ('dunning_policy', 'dunning-policy', 'platform'),
    ('product_price_version', 'product-price-version', 'platform'),
    ('commercial_product', 'commercial-product', 'platform')
) AS a(legacy, aggregate, tenant_source) ON a.legacy = o.aggregate_type
CROSS JOIN LATERAL (
    SELECT CASE a.tenant_source
        WHEN 'org' THEN o.tenant_id::text
        WHEN 'account' THEN (SELECT ca.organization_id::text FROM commercial_accounts ca
                             WHERE ca.commercial_account_id = o.tenant_id)
        WHEN 'statement' THEN (SELECT e.organization_id::text FROM usage_statements us
                               JOIN usage_event_records e ON e.subscription_id = us.subscription_id
                               WHERE us.statement_id = o.aggregate_id LIMIT 1)
        WHEN 'adjustment' THEN (SELECT e.organization_id::text FROM usage_adjustments ua
                                JOIN usage_event_records e ON e.meter_key = ua.meter_key
                                    AND e.usage_event_id = ua.source_usage_event_id
                                WHERE ua.adjustment_id = o.aggregate_id)
        WHEN 'platform' THEN '00000000-0000-0000-0000-00000000f001'
    END AS tenant
) AS t
WHERE o.published_at IS NULL;
