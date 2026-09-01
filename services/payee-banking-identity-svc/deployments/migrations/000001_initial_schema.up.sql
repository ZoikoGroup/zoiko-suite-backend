CREATE TABLE payee_destinations (
    destination_id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NULL,
    legal_entity_id      TEXT NOT NULL,
    party_ref            TEXT NOT NULL,
    scope                TEXT NOT NULL DEFAULT 'DEFAULT',
    financial_institution TEXT NOT NULL,
    account_identifier   TEXT NOT NULL,
    account_last4        TEXT NOT NULL,
    country_code         TEXT NOT NULL,
    currency             TEXT NOT NULL,
    payee_name           TEXT NOT NULL,
    source_type          TEXT NOT NULL
                         CHECK (source_type IN ('SUPPLIER_PORTAL', 'INVOICE_OCR', 'EMAIL', 'MANUAL_ENTRY')),
    fingerprint          TEXT NOT NULL,

    status               TEXT NOT NULL DEFAULT 'CANDIDATE'
                         CHECK (status IN ('CANDIDATE', 'VERIFICATION_PENDING', 'VERIFIED', 'APPROVAL_PENDING',
                                           'ACTIVE', 'SUSPENDED', 'SUPERSEDED')),

    verification_method       TEXT NOT NULL DEFAULT '',
    verification_evidence_ref TEXT NOT NULL DEFAULT '',
    verified_by_principal_id  TEXT NOT NULL DEFAULT '',
    verified_at               TIMESTAMPTZ NULL,

    approved_by_principal_id TEXT NOT NULL DEFAULT '',
    approved_at              TIMESTAMPTZ NULL,

    superseded_by_destination_id UUID NULL,
    suspend_reason                TEXT NOT NULL DEFAULT '',

    proposed_by_principal_id TEXT NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_payee_destinations_party ON payee_destinations (party_ref);
CREATE INDEX idx_payee_destinations_legal_entity ON payee_destinations (legal_entity_id);

-- The literal fix for the spec's own "destination candidate fingerprint
-- detects duplicates" requirement — a real database uniqueness
-- constraint, scoped to non-superseded rows so a genuinely retired
-- destination doesn't block a legitimate later re-proposal of the same
-- real-world account.
CREATE UNIQUE INDEX uq_payee_destinations_fingerprint
    ON payee_destinations (party_ref, fingerprint)
    WHERE status <> 'SUPERSEDED';

-- The literal fix for "only one active version per party/scope" — a real
-- database invariant, not an application-level check a race could defeat.
CREATE UNIQUE INDEX uq_payee_destinations_active_scope
    ON payee_destinations (party_ref, scope)
    WHERE status = 'ACTIVE';

CREATE TABLE payee_destination_events (
    event_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NULL,
    destination_id     UUID NOT NULL REFERENCES payee_destinations (destination_id),
    event_type         TEXT NOT NULL,
    detail             TEXT NOT NULL DEFAULT '',
    actor_principal_id TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_payee_destination_events_destination ON payee_destination_events (destination_id);
