CREATE TABLE supplier_recovery_cases (
    case_id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    legal_entity_id    TEXT NOT NULL,
    supplier_ref       TEXT NOT NULL,
    recovery_basis     TEXT NOT NULL
                       CHECK (recovery_basis IN ('OVERPAYMENT', 'DUPLICATE_PAYMENT', 'SUPPLIER_CREDIT', 'CONTRACTUAL')),
    source_payable_id  TEXT NOT NULL,

    total_amount       NUMERIC(18, 2) NOT NULL CHECK (total_amount > 0),
    recovered_amount   NUMERIC(18, 2) NOT NULL DEFAULT 0 CHECK (recovered_amount >= 0),
    currency           TEXT NOT NULL,
    recovery_reason    TEXT NOT NULL DEFAULT '',

    status             TEXT NOT NULL DEFAULT 'OPEN'
                       CHECK (status IN ('OPEN', 'APPROVED', 'IN_RECOVERY', 'PARTIALLY_RECOVERED',
                                         'RECOVERED', 'CLOSED', 'ESCALATED', 'WRITTEN_OFF')),

    escalation_reason  TEXT NOT NULL DEFAULT '',
    write_off_reason   TEXT NOT NULL DEFAULT '',
    close_note         TEXT NOT NULL DEFAULT '',

    created_by_principal_id  TEXT NOT NULL,
    approved_by_principal_id TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_supplier_recovery_cases_legal_entity ON supplier_recovery_cases (legal_entity_id);
CREATE INDEX idx_supplier_recovery_cases_supplier ON supplier_recovery_cases (supplier_ref);
CREATE INDEX idx_supplier_recovery_cases_source_payable ON supplier_recovery_cases (source_payable_id);

CREATE TABLE recovery_applications (
    application_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    case_id            UUID NOT NULL REFERENCES supplier_recovery_cases (case_id),
    application_type   TEXT NOT NULL CHECK (application_type IN ('OFFSET', 'REFUND')),
    amount             NUMERIC(18, 2) NOT NULL CHECK (amount > 0),
    idempotency_ref    TEXT NOT NULL,
    detail             TEXT NOT NULL DEFAULT '',
    actor_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_recovery_applications_case ON recovery_applications (case_id);

-- The literal fix for negative-path scenario analogous to AP-08's own
-- "confirmed payment applied twice": a repeat call naming the same real
-- bank statement line (REFUND) or the same offset reference (OFFSET)
-- against the same case is idempotent, never double-applied.
CREATE UNIQUE INDEX uq_recovery_applications_ref ON recovery_applications (case_id, application_type, idempotency_ref);

CREATE TABLE recovery_commitments (
    commitment_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    case_id            UUID NOT NULL REFERENCES supplier_recovery_cases (case_id),
    detail             TEXT NOT NULL,
    expected_method    TEXT NOT NULL DEFAULT '',
    actor_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_recovery_commitments_case ON recovery_commitments (case_id);
