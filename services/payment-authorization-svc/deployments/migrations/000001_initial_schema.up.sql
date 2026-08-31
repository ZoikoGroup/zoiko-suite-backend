CREATE TABLE payment_authorizations (
    authorization_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NULL,
    legal_entity_id          UUID NOT NULL,
    proposal_id              UUID NOT NULL,
    proposal_fingerprint     TEXT NOT NULL,
    net_amount               NUMERIC(18, 2) NOT NULL CHECK (net_amount > 0),
    currency                 TEXT NOT NULL,

    status                   TEXT NOT NULL DEFAULT 'PENDING'
                             CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'CONSUMED',
                                                'REVOKED', 'EXPIRED', 'INVALIDATED')),

    policy_assessment_result TEXT NOT NULL DEFAULT '',
    policy_version_id        TEXT NOT NULL DEFAULT '',

    requested_by_principal_id TEXT NOT NULL,

    approved_by_principal_id TEXT NULL,
    approved_at              TIMESTAMPTZ NULL,
    rejected_reason          TEXT NOT NULL DEFAULT '',

    revoked_by_principal_id  TEXT NULL,
    revoked_reason           TEXT NOT NULL DEFAULT '',
    revoked_at               TIMESTAMPTZ NULL,

    expired_at               TIMESTAMPTZ NULL,

    consumed_by_principal_id TEXT NULL,
    consumed_at              TIMESTAMPTZ NULL,

    invalidated_reason       TEXT NOT NULL DEFAULT '',

    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_payment_authorizations_tenant ON payment_authorizations (tenant_id);
CREATE INDEX idx_payment_authorizations_proposal ON payment_authorizations (proposal_id);

-- A frozen proposal may have at most one active (non-terminal) authorization
-- request in flight at a time — a real, cheap invariant beyond AP-10's own
-- four named negative-path scenarios, using the same partial-unique-index
-- technique as AP-09's active-payable uniqueness.
CREATE UNIQUE INDEX uq_payment_authorizations_active_proposal
    ON payment_authorizations (proposal_id)
    WHERE status IN ('PENDING', 'APPROVED');

CREATE TABLE authorization_payee_snapshots (
    snapshot_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NULL,
    authorization_id      UUID NOT NULL REFERENCES payment_authorizations (authorization_id),
    payee_ref             TEXT NOT NULL,
    payee_snapshot_at     TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_authorization_payee_snapshots_auth ON authorization_payee_snapshots (authorization_id);

CREATE TABLE authorization_events (
    event_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    authorization_id   UUID NOT NULL REFERENCES payment_authorizations (authorization_id),
    event_type         TEXT NOT NULL,
    detail             TEXT NOT NULL DEFAULT '',
    actor_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_authorization_events_auth ON authorization_events (authorization_id, created_at ASC);
