CREATE TABLE payment_proposals (
    proposal_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NULL,
    legal_entity_id         UUID NOT NULL,
    paying_bank_account_ref TEXT NOT NULL,
    currency                TEXT NOT NULL,
    payment_date            TIMESTAMPTZ NOT NULL,
    payment_method          TEXT NOT NULL,

    status                  TEXT NOT NULL DEFAULT 'DRAFT'
                            CHECK (status IN ('DRAFT', 'CALCULATED', 'REVIEW', 'FROZEN',
                                               'AUTHORIZED', 'REJECTED', 'CANCELLED')),

    gross_amount            NUMERIC(18, 2) NOT NULL DEFAULT 0,
    withholding_amount      NUMERIC(18, 2) NOT NULL DEFAULT 0,
    net_amount              NUMERIC(18, 2) NOT NULL DEFAULT 0,

    frozen_by_principal_id  TEXT NULL,
    frozen_at               TIMESTAMPTZ NULL,
    cancel_reason           TEXT NOT NULL DEFAULT '',

    created_by_principal_id TEXT NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_payment_proposals_tenant ON payment_proposals (tenant_id);

CREATE TABLE proposal_items (
    item_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NULL,
    proposal_id         UUID NOT NULL REFERENCES payment_proposals (proposal_id),
    payable_source      TEXT NOT NULL CHECK (payable_source IN ('AP_INVOICE', 'EXPENSE_CLAIM')),
    payable_id          TEXT NOT NULL,
    payee_ref           TEXT NOT NULL,

    gross_amount        NUMERIC(18, 2) NOT NULL CHECK (gross_amount > 0),
    withholding_amount  NUMERIC(18, 2) NOT NULL DEFAULT 0,
    net_amount          NUMERIC(18, 2) NOT NULL,
    currency            TEXT NOT NULL,
    due_date            TIMESTAMPTZ NOT NULL,

    payee_snapshot_at   TIMESTAMPTZ NULL,
    tax_determination_id TEXT NOT NULL DEFAULT '',
    exception_ref       TEXT NOT NULL DEFAULT '',
    is_active           BOOLEAN NOT NULL DEFAULT TRUE,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_proposal_items_proposal ON proposal_items (proposal_id);

-- Negative-path scenario #2: the same payable can never be an active item
-- on more than one proposal at a time, platform-wide — a genuine database
-- invariant, not an application-level check a race could defeat.
-- CancelPaymentProposal flips is_active to FALSE on all its items (in the
-- same transaction as cancelling the proposal itself), which is what frees
-- a payable for re-selection into a different proposal.
CREATE UNIQUE INDEX uq_proposal_items_active_payable
    ON proposal_items (payable_source, payable_id)
    WHERE is_active;

CREATE TABLE proposal_events (
    event_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    proposal_id        UUID NOT NULL REFERENCES payment_proposals (proposal_id),
    event_type         TEXT NOT NULL,
    detail             TEXT NOT NULL DEFAULT '',
    actor_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_proposal_events_proposal ON proposal_events (proposal_id, created_at ASC);
