-- Migration: 000008_migration_certificates.up.sql
--
-- A MigrationCertificate is the evidence/certificate this service exists to
-- issue (see services/migration-integrity-svc's package doc): the claim
-- that a named scope of migrated records (one or more migration_batch_refs,
-- i.e. one or more general-ledger-svc OPENING batches) loaded completely and
-- correctly. It is issued only once, by one named principal, and it is
-- backed by general-ledger-svc's OWN reconciliation result -- gl_journal_id /
-- gl_journal_number / gl_evidence_hash are copied from GL's
-- GetMigrationEvidence response at issuance time, never computed here. See
-- the handler for the issuance preconditions (clean validation or formally
-- accepted exceptions, AND GL Post+Reconcile both succeeded).
CREATE TABLE IF NOT EXISTS migration_certificates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,

    scope VARCHAR(256) NOT NULL,       -- human-readable scope statement, e.g. "FY25 opening balances, Acme GmbH"
    batch_ref_id UUID NOT NULL REFERENCES migration_batch_refs(id),
    job_id UUID NOT NULL REFERENCES migration_jobs(id),

    unresolved_exceptions INT NOT NULL DEFAULT 0,

    -- The general-ledger-svc evidence this certificate's claim rests on,
    -- read fresh at issuance (never asserted independently of GL).
    gl_batch_id VARCHAR(64) NOT NULL,
    gl_journal_id VARCHAR(64) NOT NULL,
    gl_journal_number VARCHAR(128) NOT NULL,
    gl_reconciliation_status VARCHAR(16) NOT NULL,
    gl_evidence_hash VARCHAR(64) NOT NULL,
    gl_evidence_verified BOOLEAN NOT NULL,

    signer_principal_id VARCHAR(256) NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT migration_certificates_recon_valid
        CHECK (gl_reconciliation_status = 'MATCHED'),
    -- One certificate per promoted batch: re-issuing would be a second,
    -- competing evidence record for the same cutover.
    CONSTRAINT migration_certificates_batch_ref_unique UNIQUE (tenant_id, batch_ref_id)
);

ALTER TABLE migration_certificates ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS migration_certificates_isolation ON migration_certificates;
CREATE POLICY migration_certificates_isolation ON migration_certificates
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX IF NOT EXISTS idx_migration_certificates_entity ON migration_certificates(tenant_id, legal_entity_id);
