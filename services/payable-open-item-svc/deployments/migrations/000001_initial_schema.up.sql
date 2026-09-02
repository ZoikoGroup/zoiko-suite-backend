CREATE TABLE payable_open_items (
    payable_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    legal_entity_id    TEXT NOT NULL,
    source_type        TEXT NOT NULL
                       CHECK (source_type IN ('EXPENSE_CLAIM', 'SUPPLIER_INVOICE', 'AUTHORIZED_ADJUSTMENT')),
    source_reference   TEXT NOT NULL,
    payee_ref          TEXT NOT NULL,

    original_amount    NUMERIC(18, 2) NOT NULL CHECK (original_amount > 0),
    residual_amount    NUMERIC(18, 2) NOT NULL,
    currency           TEXT NOT NULL,
    due_date           TIMESTAMPTZ NOT NULL,

    status             TEXT NOT NULL DEFAULT 'OPEN'
                       CHECK (status IN ('OPEN', 'PARTIALLY_SETTLED', 'SETTLED')),

    is_held            BOOLEAN NOT NULL DEFAULT FALSE,
    hold_reason        TEXT NOT NULL DEFAULT '',

    is_disputed        BOOLEAN NOT NULL DEFAULT FALSE,
    dispute_reason     TEXT NOT NULL DEFAULT '',
    dispute_opened_at  TIMESTAMPTZ NULL,

    closed_at          TIMESTAMPTZ NULL,

    created_by_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_payable_open_items_legal_entity ON payable_open_items (legal_entity_id);
CREATE INDEX idx_payable_open_items_payee ON payable_open_items (payee_ref);

-- The literal fix for negative-path #4 ("AP totals match GL but
-- duplicate/missing open items remain"): a real database uniqueness
-- constraint, not an application-level check a race could defeat.
CREATE UNIQUE INDEX uq_payable_open_items_source ON payable_open_items (source_type, source_reference);

CREATE TABLE settlement_applications (
    application_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    payable_id         UUID NOT NULL REFERENCES payable_open_items (payable_id),
    application_type   TEXT NOT NULL CHECK (application_type IN ('PAYMENT', 'SUPPLIER_CREDIT', 'RECOVERY')),
    amount             NUMERIC(18, 2) NOT NULL,
    idempotency_ref    TEXT NOT NULL DEFAULT '',
    detail             TEXT NOT NULL DEFAULT '',
    actor_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_settlement_applications_payable ON settlement_applications (payable_id);

-- The literal fix for negative-path #3 ("confirmed payment applied
-- twice"): a repeat call naming the same real-world payment reference is
-- idempotent, never double-applied. Scoped to PAYMENT applications only —
-- a blank idempotency_ref (e.g. an ad hoc supplier credit) is exempt via
-- the partial index.
CREATE UNIQUE INDEX uq_settlement_applications_payment_ref
    ON settlement_applications (payable_id, idempotency_ref)
    WHERE application_type = 'PAYMENT' AND idempotency_ref <> '';
