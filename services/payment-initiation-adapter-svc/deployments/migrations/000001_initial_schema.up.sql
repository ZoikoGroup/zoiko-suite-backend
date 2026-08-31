CREATE TABLE payment_initiation_attempts (
    attempt_id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NULL,
    legal_entity_id           UUID NOT NULL,
    source_reference          TEXT NOT NULL,
    authorization_fingerprint TEXT NOT NULL,
    payer_account_ref         TEXT NOT NULL,
    payee_ref                 TEXT NOT NULL,
    amount                    NUMERIC(18, 2) NOT NULL CHECK (amount > 0),
    currency                  TEXT NOT NULL,
    execution_date            TIMESTAMPTZ NOT NULL,
    payment_reference         TEXT NOT NULL DEFAULT '',
    payer_account_verified    BOOLEAN NOT NULL DEFAULT FALSE,

    idempotency_key           TEXT NOT NULL,

    status                    TEXT NOT NULL DEFAULT 'PREPARED'
                              CHECK (status IN ('PREPARED', 'SUBMITTED', 'PENDING_UNKNOWN',
                                                 'REJECTED_BEFORE_SUBMISSION', 'CANCELLED', 'QUARANTINED')),
    provider_request_id       TEXT NOT NULL DEFAULT '',
    provider_response_ref     TEXT NOT NULL DEFAULT '',
    rejection_reason          TEXT NOT NULL DEFAULT '',
    quarantine_reason         TEXT NOT NULL DEFAULT '',
    ambiguous_resolution_note TEXT NOT NULL DEFAULT '',

    submitted_at              TIMESTAMPTZ NULL,
    resolved_at               TIMESTAMPTZ NULL,

    created_by_principal_id   TEXT NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_payment_initiation_attempts_tenant ON payment_initiation_attempts (tenant_id);

-- Negative-path: "provider timeout triggers new payment ID" must be
-- structurally impossible — one idempotency key can only ever back one
-- attempt, platform-wide.
CREATE UNIQUE INDEX uq_payment_initiation_attempts_idempotency_key ON payment_initiation_attempts (idempotency_key);

CREATE TABLE attempt_events (
    event_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    attempt_id         UUID NOT NULL REFERENCES payment_initiation_attempts (attempt_id),
    event_type         TEXT NOT NULL,
    detail             TEXT NOT NULL DEFAULT '',
    actor_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_attempt_events_attempt ON attempt_events (attempt_id, created_at ASC);
