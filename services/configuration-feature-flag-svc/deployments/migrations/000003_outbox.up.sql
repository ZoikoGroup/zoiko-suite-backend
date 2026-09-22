-- configuration-feature-flag-svc: a transactional outbox for the two
-- configuration events.
--
-- ─────────────────────────────────────────────────────────────────────────────
-- WHY
--
-- Until now config.updated and feature_flag.updated were written straight to
-- Kafka from the handler, AFTER the store transaction had already committed,
-- with the error logged and discarded:
--
--     if pubErr := h.publisher.PublishConfigUpdated(...); pubErr != nil {
--         h.log.Error("failed to publish config.updated", ...)
--     }
--     writeJSON(w, 201, entry)
--
-- So a broker hiccup during a write left: the new version correctly recorded
-- and correctly returned 201, the operator correctly told the change was
-- saved — and every consumer still holding the PREVIOUS value, with no error
-- anywhere and nothing in the metrics to show for it.
--
-- That matters more here than on most services. This service exists so other
-- services can change behaviour at runtime without a redeploy; a consumer that
-- caches a config value and never learns it changed is the exact failure the
-- events exist to prevent. And because the value it is serving is perfectly
-- valid — just superseded — nothing downstream can detect the staleness
-- either. The rollout percentage a consumer enforces silently stops matching
-- the one this service shows the operator.
--
-- Writing the event in the SAME transaction as the version change makes the
-- two atomic: a value that changed always has its event, and an event that
-- exists always has its change. Delivery then becomes a separate, retryable
-- problem that internal/outbox owns and that the metrics make visible.
--
-- Mirrors access-control-svc's 000004 exactly, including the RLS shape — see
-- that file for the reasoning behind the relay's named escape hatch.

CREATE TABLE IF NOT EXISTS event_outbox (
    outbox_id       BIGSERIAL PRIMARY KEY,

    -- TEXT, not UUID, and deliberately nullable-as-empty rather than NULL.
    --
    -- A global config entry has tenant_id IS NULL — "the default for every
    -- tenant in this environment" — which is a real scope, not a missing
    -- value. The RLS predicate below compares this column against a GUC that
    -- is itself a string, so a global row carries '' and is matched by the
    -- relay disjunct alone. Keeping the column NOT NULL means the policy never
    -- has to reason about NULL = NULL.
    tenant_id       VARCHAR(255) NOT NULL,

    event_type      VARCHAR(100) NOT NULL,

    -- The Kafka partition key: the config_id or flag_id the event is about.
    -- Text rather than UUID because the two aggregates have different id
    -- columns and this table does not need to know which one it is holding.
    aggregate_key   VARCHAR(255) NOT NULL,

    -- The fully-built envelope, exactly as it will be written to Kafka. Built
    -- at write time rather than at publish time on purpose: the envelope
    -- carries the actor and the correlation id of the request that caused the
    -- change, and the relay runs long after that request context is gone.
    payload         JSONB NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,

    -- The only list a deployment can actually violate: an event type this
    -- constraint omits fails the INSERT, and because the enqueue shares the
    -- write's transaction, it fails the WRITE. Keep in step with
    -- internal/events and asyncapi.yaml.
    CONSTRAINT event_outbox_event_known
        CHECK (event_type IN ('config.updated', 'feature_flag.updated'))
);

-- The relay's claim query, and the only index it needs: unpublished rows,
-- oldest first. Partial, so the index stays the size of the BACKLOG rather
-- than the size of the history.
CREATE INDEX IF NOT EXISTS idx_event_outbox_unpublished
    ON event_outbox (created_at, outbox_id) WHERE published_at IS NULL;

ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;

-- Tenant isolation, with one deliberate escape hatch.
--
-- Every request-path write installs app.tenant_id and is confined to it. The
-- relay is not a request: it drains every tenant's backlog from one background
-- loop, so it installs app.outbox_relay instead and is admitted by the second
-- disjunct. Rather than letting it run unscoped — which under FORCE ROW LEVEL
-- SECURITY would simply see nothing, and would present as a relay that
-- publishes nothing while reporting no error at all — it names itself, so the
-- policy admits it by an explicit, auditable disjunct instead of by the
-- absence of a control.
--
-- NULLIF is not decoration. Postgres keeps a custom GUC in the SESSION after a
-- transaction-local SET is reset, with '' as its value, so on any pooled
-- connection that has already served one request these settings read as the
-- empty string rather than NULL.
-- Dropped first so the whole migration is re-runnable, matching the
-- IF NOT EXISTS on the table above. CREATE POLICY has no IF NOT EXISTS, so
-- without this a re-run fails at 42710 and a half-applied migration has to be
-- unpicked by hand.
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
