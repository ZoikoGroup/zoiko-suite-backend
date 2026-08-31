CREATE TABLE payment_execution_states (
    payment_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NULL,
    legal_entity_id      UUID NOT NULL,
    provider_request_id  TEXT NOT NULL DEFAULT '',
    source_reference     TEXT NOT NULL DEFAULT '',

    status               TEXT NOT NULL DEFAULT 'PREPARED'
                         CHECK (status IN ('PREPARED', 'SUBMITTED', 'ACCEPTED', 'PENDING', 'SETTLED',
                                            'REJECTED', 'RETURNED', 'CANCELLED')),

    finality_source      TEXT NOT NULL DEFAULT '',
    mapping_version      TEXT NOT NULL DEFAULT '',

    has_open_conflict    BOOLEAN NOT NULL DEFAULT FALSE,
    conflict_reason      TEXT NOT NULL DEFAULT '',

    created_by_principal_id TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_payment_execution_states_tenant ON payment_execution_states (tenant_id);
CREATE INDEX idx_payment_execution_states_provider_request ON payment_execution_states (provider_request_id);

-- Negative-path scenario #3 ("duplicate callback creates duplicate
-- accounting effect"): a real provider event, identified by its own
-- reference, may only ever be applied once per payment — a genuine
-- database uniqueness constraint, not an application check a race could
-- defeat.
CREATE TABLE status_events (
    event_id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NULL,
    payment_id          UUID NOT NULL REFERENCES payment_execution_states (payment_id),
    event_type          TEXT NOT NULL,
    from_status         TEXT NOT NULL DEFAULT '',
    to_status           TEXT NOT NULL DEFAULT '',
    provider_event_ref  TEXT NOT NULL DEFAULT '',
    detail              TEXT NOT NULL DEFAULT '',
    actor_principal_id  TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_status_events_provider_event_ref
    ON status_events (payment_id, provider_event_ref)
    WHERE provider_event_ref <> '';
CREATE INDEX idx_status_events_payment ON status_events (payment_id, created_at ASC);
