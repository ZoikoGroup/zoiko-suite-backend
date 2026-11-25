-- ZS-SVC-Z-001 DQC-05 / RB-05 / DQC-017 (docs/architecture/DQC_GAP_MATRIX.md, step F3).
--
-- A certification is sealed and immutable, and had no way to be withdrawn: a suspected false PASS
-- could be noticed and not acted on. Revocation is a SEPARATE append-only record, so the sealed
-- certification, its summary and its hash are preserved byte for byte ("preserve prior evidence"),
-- and revocation is PROSPECTIVE: it states from when the certification is no longer to be relied on.
-- One revocation per certification, never edited or removed. The certification's basis stays
-- claimed, so the same evidence cannot simply be certified again; the remedy is new evidence.

-- True when every reference is present and not blank. A CHECK cannot hold a subquery, so it lives here.
CREATE OR REPLACE FUNCTION dq_refs_nonblank(refs TEXT[]) RETURNS BOOLEAN AS $$
    SELECT bool_and(r IS NOT NULL AND length(btrim(r)) > 0) FROM unnest(refs) AS r;
$$ LANGUAGE sql IMMUTABLE;

 CREATE TABLE IF NOT EXISTS dq_certification_revocations (
    revocation_id     TEXT         PRIMARY KEY
        CHECK (revocation_id ~ '^dqx_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id         VARCHAR(64)  NOT NULL,
    certification_id  TEXT         NOT NULL REFERENCES dq_certifications(certification_id),
    reason_code       VARCHAR(32)  NOT NULL
        CHECK (reason_code IN ('FALSE_PASS', 'SOURCE_DEFECT', 'CONTROL_DEFECT', 'EVIDENCE_INVALID', 'OTHER')),
    reason            TEXT         NOT NULL CHECK (length(btrim(reason)) > 0),
    -- "Certification revoked with no reason/evidence" is a named negative path (ZS-SVC-Z-001 NP): the new
    -- material evidence the revocation rests on is mandatory, as it is for reconciliation-engine-svc's.
    evidence_refs     TEXT[]       NOT NULL
        CHECK (cardinality(evidence_refs) > 0 AND dq_refs_nonblank(evidence_refs)),
    -- Who certified and when, copied from the sealed certification so the record stands alone.
    certified_by      VARCHAR(128) NOT NULL,
    certified_at      TIMESTAMPTZ  NOT NULL,
    revoked_by        VARCHAR(128) NOT NULL,
    revoked_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (certification_id),
    -- Independence: whoever certified cannot be the one who revokes (as in reconciliation-engine-svc).
    CONSTRAINT dq_cert_revocation_independent CHECK (revoked_by <> certified_by),
    -- Prospective: a revocation takes effect after the certification it withdraws, never before it.
    CONSTRAINT dq_cert_revocation_prospective CHECK (revoked_at >= certified_at)
);

CREATE INDEX IF NOT EXISTS idx_dq_cert_revocations_tenant ON dq_certification_revocations(tenant_id);

CREATE OR REPLACE FUNCTION enforce_dq_revocation_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dq certification revocation is immutable and cannot be changed or deleted';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_dq_revocation_immutability ON dq_certification_revocations;
CREATE TRIGGER trigger_dq_revocation_immutability
    BEFORE UPDATE OR DELETE ON dq_certification_revocations
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_revocation_immutability();

ALTER TABLE dq_certification_revocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_certification_revocations FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS dq_cert_revocations_tenant_isolation ON dq_certification_revocations;
CREATE POLICY dq_cert_revocations_tenant_isolation ON dq_certification_revocations
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
