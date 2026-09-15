-- AUD-06 Audit Evidence — a parallel table set referencing documents/
-- document_versions by ID, not new columns on those shared tables. See
-- internal/domain/evidence.go's own package doc for why.

CREATE TABLE audit_evidence (
    evidence_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id UUID NOT NULL REFERENCES documents(document_id),
    tenant_id UUID NOT NULL,
    legal_entity_id UUID NOT NULL,
    engagement_id UUID NOT NULL,
    -- "Evidence source and acquisition method mandatory" — non-empty,
    -- not merely NOT NULL: an empty string satisfies NOT NULL but not
    -- the doc's own requirement that these be real, present facts.
    evidence_source TEXT NOT NULL CHECK (evidence_source <> ''),
    acquisition_method TEXT NOT NULL CHECK (acquisition_method <> ''),
    status_flags JSONB NOT NULL DEFAULT '{}',
    registered_by_principal_id VARCHAR(255) NOT NULL,
    correlation_id VARCHAR(255) NOT NULL,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_evidence_tenant ON audit_evidence (tenant_id, legal_entity_id);
CREATE INDEX idx_audit_evidence_document ON audit_evidence (document_id);
CREATE UNIQUE INDEX audit_evidence_create_idempotency_unique ON audit_evidence (tenant_id, correlation_id);

CREATE TABLE evidence_versions (
    evidence_version_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    evidence_id UUID NOT NULL REFERENCES audit_evidence(evidence_id),
    tenant_id UUID NOT NULL,
    document_version_id UUID NOT NULL REFERENCES document_versions(document_version_id),
    superseded_by_evidence_version_id UUID REFERENCES evidence_versions(evidence_version_id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE evidence_reliability_assessments (
    assessment_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    evidence_id UUID NOT NULL REFERENCES audit_evidence(evidence_id),
    tenant_id UUID NOT NULL,
    assessed_by_principal_id VARCHAR(255) NOT NULL,
    reliability_rating TEXT NOT NULL,
    rationale TEXT NOT NULL DEFAULT '',
    assessed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE evidence_procedure_links (
    link_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    evidence_id UUID NOT NULL REFERENCES audit_evidence(evidence_id),
    tenant_id UUID NOT NULL,
    procedure_ref TEXT NOT NULL,
    linked_by_principal_id VARCHAR(255) NOT NULL,
    linked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- No DELETE path exists anywhere in the store for this table — the
-- absence itself is AUD-NEG-021's own mechanism. See the domain package
-- doc comment on EvidenceContradiction.
CREATE TABLE evidence_contradictions (
    contradiction_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    evidence_id UUID NOT NULL REFERENCES audit_evidence(evidence_id),
    tenant_id UUID NOT NULL,
    contradicting_evidence_id UUID REFERENCES audit_evidence(evidence_id),
    description TEXT NOT NULL,
    recorded_by_principal_id VARCHAR(255) NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE custody_entries (
    custody_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    evidence_id UUID NOT NULL REFERENCES audit_evidence(evidence_id),
    tenant_id UUID NOT NULL,
    action TEXT NOT NULL,
    actor_principal_id VARCHAR(255) NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE audit_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON audit_evidence
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE evidence_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_versions
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE evidence_reliability_assessments ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_reliability_assessments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_reliability_assessments
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE evidence_procedure_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_procedure_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_procedure_links
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE evidence_contradictions ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_contradictions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_contradictions
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

ALTER TABLE custody_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE custody_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON custody_entries
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- Append-only doctrine, same idiom as documents/document_versions'
-- own immutability triggers (migration 000002).
CREATE OR REPLACE FUNCTION reject_evidence_reliability_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'evidence reliability assessments are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_evidence_reliability_mutation
    BEFORE UPDATE OR DELETE ON evidence_reliability_assessments
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_reliability_mutation();

CREATE OR REPLACE FUNCTION reject_evidence_procedure_link_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'evidence procedure links are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_evidence_procedure_link_mutation
    BEFORE UPDATE OR DELETE ON evidence_procedure_links
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_procedure_link_mutation();

-- "No deletion" is enforced both by application code (no DELETE route)
-- and, defense-in-depth, at the DB layer: this table permits no UPDATE or
-- DELETE at all, ever — this is the literal mechanism behind AUD-NEG-021.
CREATE OR REPLACE FUNCTION reject_evidence_contradiction_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'evidence contradictions are never modified or deleted — see AUD-NEG-021';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_evidence_contradiction_mutation
    BEFORE UPDATE OR DELETE ON evidence_contradictions
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_contradiction_mutation();

CREATE OR REPLACE FUNCTION reject_custody_entry_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'custody entries are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_custody_entry_mutation
    BEFORE UPDATE OR DELETE ON custody_entries
    FOR EACH ROW EXECUTE FUNCTION reject_custody_entry_mutation();

-- Only superseded_by_evidence_version_id may ever change on an existing
-- evidence_versions row — every other column is permanent once written.
CREATE OR REPLACE FUNCTION reject_evidence_version_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evidence versions are never deleted';
    END IF;
    IF NEW.evidence_id IS DISTINCT FROM OLD.evidence_id
        OR NEW.document_version_id IS DISTINCT FROM OLD.document_version_id THEN
        RAISE EXCEPTION 'evidence version content is immutable — only superseded_by_evidence_version_id may be set';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_evidence_version_mutation
    BEFORE UPDATE OR DELETE ON evidence_versions
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_version_mutation();
