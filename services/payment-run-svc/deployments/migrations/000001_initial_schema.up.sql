CREATE TABLE payment_runs (
    run_id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NULL,
    legal_entity_id          UUID NOT NULL,
    paying_bank_account_ref  TEXT NOT NULL,
    currency                 TEXT NOT NULL,
    value_date               TIMESTAMPTZ NOT NULL,
    payment_method           TEXT NOT NULL,

    status                   TEXT NOT NULL DEFAULT 'DRAFT'
                             CHECK (status IN ('DRAFT', 'VALIDATED', 'LOCKED', 'SUBMITTED', 'ACCEPTED',
                                                'REJECTED', 'PARTIALLY_ACCEPTED', 'SETTLED', 'COMPLETED',
                                                'EXCEPTION', 'CANCELLED')),
    idempotency_key          TEXT NOT NULL DEFAULT '',

    created_by_principal_id TEXT NOT NULL,
    validated_at             TIMESTAMPTZ NULL,
    locked_at                TIMESTAMPTZ NULL,
    submitted_at             TIMESTAMPTZ NULL,
    closed_at                TIMESTAMPTZ NULL,
    exception_reason         TEXT NOT NULL DEFAULT '',
    cancel_reason            TEXT NOT NULL DEFAULT '',
    close_note               TEXT NOT NULL DEFAULT '',

    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_payment_runs_tenant ON payment_runs (tenant_id);

CREATE TABLE run_instructions (
    instruction_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    run_id             UUID NOT NULL REFERENCES payment_runs (run_id),
    authorization_id   TEXT NOT NULL,
    payee_ref          TEXT NOT NULL,
    net_amount         NUMERIC(18, 2) NOT NULL CHECK (net_amount > 0),
    currency           TEXT NOT NULL,

    status             TEXT NOT NULL DEFAULT 'PENDING'
                       CHECK (status IN ('PENDING', 'ACCEPTED', 'REJECTED', 'SETTLED', 'EXCEPTION')),
    consumed_at        TIMESTAMPTZ NULL,

    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_run_instructions_run ON run_instructions (run_id);

-- An authorization consumed into one run instruction can never be included
-- in another — a real, cheap invariant beyond AP-11's own four named
-- negative-path scenarios (AP-10's authorizations are already single-use,
-- but this is AP-11's own defense-in-depth layer of the same guarantee).
CREATE UNIQUE INDEX uq_run_instructions_authorization ON run_instructions (authorization_id);

-- Negative-path scenario #2 ("provider callback forged/replayed"),
-- reinterpreted honestly for a service with no real callback receiver (see
-- internal/domain's package doc): each distinct real-world provider event
-- (accepted, then later settled, each its own reference) may be recorded
-- exactly once per instruction. A repeat call naming the same
-- provider_event_ref is idempotent — this is what a genuine database
-- uniqueness constraint can guarantee without a signature to verify.
CREATE TABLE instruction_reconciliation_events (
    event_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NULL,
    instruction_id       UUID NOT NULL REFERENCES run_instructions (instruction_id),
    provider_event_ref   TEXT NOT NULL,
    external_status      TEXT NOT NULL CHECK (external_status IN ('ACCEPTED', 'REJECTED', 'SETTLED', 'EXCEPTION')),
    recorded_by_principal_id TEXT NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_instruction_reconciliation_events_ref
    ON instruction_reconciliation_events (instruction_id, provider_event_ref);
CREATE INDEX idx_instruction_reconciliation_events_instruction ON instruction_reconciliation_events (instruction_id, created_at ASC);

CREATE TABLE run_events (
    event_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    run_id             UUID NOT NULL REFERENCES payment_runs (run_id),
    event_type         TEXT NOT NULL,
    detail             TEXT NOT NULL DEFAULT '',
    actor_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_run_events_run ON run_events (run_id, created_at ASC);
