-- Migration: 000010_add_financial_snapshots.up.sql
--
-- ACC-16 (Signed Financial Snapshot): "owns Signed financial snapshots/
-- manifests. Must never own: Mutable live balances." Fuller ownership:
-- "FinancialSnapshot, manifest, content hash/signature, purpose, source
-- references and supersession chain." State model: "Draft → Sealed →
-- Certified → Superseded; sealed content never edited in place."
--
-- financial_snapshots is a normal mutable stateful row while DRAFT (the
-- one and only window in which its content may be set), then effectively
-- append-only from SEALED onward: there is no endpoint anywhere that ever
-- updates content_hash/signature/content once sealed — "sealed content
-- never edited in place" is satisfied structurally, by there being no
-- mutation path, exactly the same doctrine already applied to ACC-09's
-- rule versions and ACC-10's rate references.
--
-- Deliberately generalizes rather than duplicates this service's own
-- existing close-evidence signing (see handler.go's signEvidence/
-- CreateCloseEvidence for the period-close-specific precursor this
-- capability generalizes): ACC-16 accepts a caller-declared `purpose`
-- (PERIOD_CLOSE is one value among several — AUDIT, REGULATORY_FILING,
-- etc.), not just the one hardcoded close-evidence use this service
-- already had.

CREATE TABLE financial_snapshots (
    snapshot_id                UUID PRIMARY KEY,
    tenant_id                    VARCHAR(255) NOT NULL,
    legal_entity_id               VARCHAR(255) NOT NULL,
    purpose                        VARCHAR(64) NOT NULL, -- e.g. PERIOD_CLOSE | AUDIT | REGULATORY_FILING
    content                         TEXT NOT NULL, -- caller-declared financial content, fixed at creation, never mutated
    source_references                TEXT NOT NULL, -- caller-declared JSON array of source refs (e.g. trial balance snapshot IDs, watermarks)
    content_hash                     VARCHAR(255),
    signature                        VARCHAR(255),
    has_unresolved_exceptions         BOOLEAN NOT NULL DEFAULT FALSE,
    status                            VARCHAR(20) NOT NULL, -- DRAFT|SEALED|CERTIFIED|SUPERSEDED
    superseded_by_snapshot_id          UUID REFERENCES financial_snapshots(snapshot_id),
    created_at                        TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id           VARCHAR(255) NOT NULL,
    sealed_at                         TIMESTAMP WITH TIME ZONE,
    certified_at                      TIMESTAMP WITH TIME ZONE,
    certified_by_principal_id         VARCHAR(255),
    certification_reason              TEXT,
    superseded_at                     TIMESTAMP WITH TIME ZONE
);

ALTER TABLE financial_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON financial_snapshots
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_financial_snapshots_entity ON financial_snapshots (tenant_id, legal_entity_id);
