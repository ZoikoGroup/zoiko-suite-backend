CREATE TABLE goods_service_receipts (
    receipt_id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                         UUID NULL,
    legal_entity_id                   UUID NOT NULL,
    purchase_order_id                 UUID NOT NULL,
    receipt_type                      TEXT NOT NULL CHECK (receipt_type IN ('GOODS', 'SERVICE')),
    quantity                          NUMERIC(18, 4) NOT NULL,
    unit_of_measure                   TEXT NOT NULL,
    amount                            NUMERIC(18, 2) NOT NULL CHECK (amount > 0),
    currency_code                     TEXT NOT NULL,
    receipt_date                      TIMESTAMPTZ NOT NULL,
    location                          TEXT NOT NULL DEFAULT '',
    inspection_result                 TEXT NOT NULL DEFAULT '',
    requires_independent_acceptance   BOOLEAN NOT NULL DEFAULT FALSE,
    tolerance_exception_ref           TEXT NOT NULL DEFAULT '',

    status                            TEXT NOT NULL DEFAULT 'DRAFT'
                                       CHECK (status IN ('DRAFT', 'PENDING_CONFIRMATION', 'CONFIRMED',
                                                          'REJECTED', 'PARTIALLY_REVERSED', 'FULLY_REVERSED')),
    rejection_reason                  TEXT NOT NULL DEFAULT '',
    reversed_amount                   NUMERIC(18, 2) NOT NULL DEFAULT 0,

    receiver_principal_id             TEXT NOT NULL,
    created_by_principal_id           TEXT NOT NULL,
    confirmed_by_principal_id         TEXT NULL,
    confirmed_at                      TIMESTAMPTZ NULL,

    created_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_gsr_purchase_order ON goods_service_receipts (purchase_order_id);
CREATE INDEX idx_gsr_tenant ON goods_service_receipts (tenant_id);

CREATE TABLE receipt_evidence (
    evidence_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NULL,
    receipt_id              UUID NOT NULL REFERENCES goods_service_receipts (receipt_id),
    evidence_ref            TEXT NOT NULL,
    description             TEXT NOT NULL DEFAULT '',
    recorded_by_principal_id TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_receipt_evidence_receipt ON receipt_evidence (receipt_id);

CREATE TABLE receipt_reversals (
    reversal_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NULL,
    receipt_id              UUID NOT NULL REFERENCES goods_service_receipts (receipt_id),
    reversed_amount         NUMERIC(18, 2) NOT NULL CHECK (reversed_amount > 0),
    reason                  TEXT NOT NULL,
    reversed_by_principal_id TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_receipt_reversals_receipt ON receipt_reversals (receipt_id);

CREATE TABLE receipt_accounting_events (
    event_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NULL,
    receipt_id       UUID NOT NULL REFERENCES goods_service_receipts (receipt_id),
    status           TEXT NOT NULL CHECK (status IN ('POSTED', 'EXCEPTION')),
    journal_id       TEXT NULL,
    failure_reason   TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_receipt_accounting_events_receipt ON receipt_accounting_events (receipt_id, created_at DESC);
