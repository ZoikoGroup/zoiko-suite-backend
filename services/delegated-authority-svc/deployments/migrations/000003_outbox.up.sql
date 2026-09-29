-- Transactional outbox for the delegation lifecycle events.
--
-- WHY THIS TABLE EXISTS
--
-- Until now the three lifecycle events were published straight to Kafka from
-- the handler, after the store transaction had already committed, with the
-- error logged and discarded. On most services that is a tolerable trade. On
-- this one it is not: authority.revoked is the signal identity-context-svc acts
-- on to end the delegate's session. A broker hiccup during a revocation left
-- the register correctly showing REVOKED, the operator correctly told the
-- revocation succeeded, and the delegate still holding a live session — with
-- nothing anywhere reporting a fault. The withdrawal of an authority is exactly
-- the event that must not be best-effort.
--
-- Writing the event in the SAME transaction as the state change makes the two
-- atomic: a grant that exists always has its event, and an event that exists
-- always has its grant. Delivery then becomes a separate, retryable problem
-- that a relay owns and that the metrics below make visible.
CREATE TABLE IF NOT EXISTS delegation_outbox (
    outbox_id       BIGSERIAL PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    delegation_id   UUID NOT NULL,
    event_type      VARCHAR(100) NOT NULL,
    -- The fully-built envelope, exactly as it will be written to Kafka. Built
    -- at write time rather than at publish time on purpose: the envelope
    -- carries the actor and the correlation of the request that caused the
    -- change, and the relay runs long after that request context is gone.
    payload         JSONB NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    published_at    TIMESTAMP WITH TIME ZONE,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    CONSTRAINT delegation_outbox_event_known
        CHECK (event_type IN ('authority.delegated', 'authority.revoked', 'authority.expired'))
);

-- The relay's claim query, and the only index it needs: unpublished rows,
-- oldest first. Partial, so the index stays the size of the BACKLOG rather than
-- the size of the history — on a healthy service that is a handful of rows
-- however many millions of events have been delivered.
CREATE INDEX IF NOT EXISTS idx_delegation_outbox_unpublished
    ON delegation_outbox (created_at, outbox_id) WHERE published_at IS NULL;

ALTER TABLE delegation_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE delegation_outbox FORCE ROW LEVEL SECURITY;

-- Tenant isolation, with one deliberate escape hatch.
--
-- Every request-path write installs app.tenant_id and is confined to it. The
-- relay is not a request: it drains every tenant's backlog from one background
-- loop, so it installs app.outbox_relay instead and is admitted by the second
-- disjunct.
--
-- NULLIF is not decoration. Postgres keeps a custom GUC in the SESSION after a
-- transaction-local SET is reset, with '' as its value, so on any pooled
-- connection that has already served one request these settings read as the
-- empty string rather than NULL. Comparing '' is harmless here because both
-- columns are text; the same expression written against a UUID column raises
-- 22P02 and surfaces a refusal as a 500. Written this way so it stays correct
-- if the column type ever changes.
CREATE POLICY outbox_tenant_isolation ON delegation_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
    );
