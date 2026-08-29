CREATE TABLE expense_claims (
    claim_id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NULL,
    legal_entity_id          UUID NOT NULL,
    claimant_principal_id    TEXT NOT NULL,
    currency                 TEXT NOT NULL,
    business_purpose         TEXT NOT NULL DEFAULT '',
    project_cost_center      TEXT NOT NULL DEFAULT '',
    payment_preference_ref   TEXT NOT NULL DEFAULT '',

    status                   TEXT NOT NULL DEFAULT 'DRAFT'
                             CHECK (status IN ('DRAFT', 'PENDING_APPROVAL', 'APPROVED', 'REJECTED',
                                                'RETURNED', 'REIMBURSABLE', 'CANCELLED')),
    rejection_reason         TEXT NOT NULL DEFAULT '',
    return_reason            TEXT NOT NULL DEFAULT '',
    has_policy_exception     BOOLEAN NOT NULL DEFAULT FALSE,
    policy_exception_reason  TEXT NOT NULL DEFAULT '',
    policy_assessment_result TEXT NOT NULL DEFAULT 'NOT_ASSESSED',
    policy_version_id        TEXT NOT NULL DEFAULT '',

    approved_by_principal_id TEXT NULL,
    approved_at              TIMESTAMPTZ NULL,

    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_expense_claims_tenant ON expense_claims (tenant_id);
CREATE INDEX idx_expense_claims_claimant ON expense_claims (claimant_principal_id);

CREATE TABLE expense_lines (
    line_id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NULL,
    claim_id              UUID NOT NULL REFERENCES expense_claims (claim_id),
    merchant              TEXT NOT NULL,
    expense_date          TIMESTAMPTZ NOT NULL,
    amount                NUMERIC(18, 2) NOT NULL CHECK (amount > 0),
    currency              TEXT NOT NULL,
    category              TEXT NOT NULL DEFAULT '',
    project_cost_center   TEXT NOT NULL DEFAULT '',
    receipt_document_id   UUID NULL,

    claim_tax_recovery    BOOLEAN NOT NULL DEFAULT FALSE,
    jurisdiction          TEXT NOT NULL DEFAULT '',
    tax_category          TEXT NOT NULL DEFAULT '',

    tax_determination_id  TEXT NOT NULL DEFAULT '',
    taxable_amount        NUMERIC(18, 2) NOT NULL DEFAULT 0,
    calculated_tax_amount NUMERIC(18, 2) NOT NULL DEFAULT 0,

    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_expense_lines_claim ON expense_lines (claim_id);

-- Negative-path scenario #2: the same receipt can never be attached to more
-- than one expense line, platform-wide, across every claim — a genuine
-- database invariant, not an application-level check a race could defeat.
-- Partial (WHERE NOT NULL) because most small expenses carry no receipt at
-- all, and NULL <> NULL in a plain UNIQUE index would already allow that,
-- but being explicit documents the intent.
CREATE UNIQUE INDEX uq_expense_lines_receipt_document
    ON expense_lines (receipt_document_id)
    WHERE receipt_document_id IS NOT NULL;

CREATE TABLE expense_claim_events (
    event_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    claim_id           UUID NOT NULL REFERENCES expense_claims (claim_id),
    event_type         TEXT NOT NULL,
    detail             TEXT NOT NULL DEFAULT '',
    actor_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_expense_claim_events_claim ON expense_claim_events (claim_id, created_at ASC);
