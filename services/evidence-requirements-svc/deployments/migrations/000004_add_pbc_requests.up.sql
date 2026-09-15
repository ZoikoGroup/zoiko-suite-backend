-- AUD-05 PBC / Evidence Request adds the request/response half this
-- service was missing: EvidenceRequirement/EvidenceEvaluation (000001)
-- answer "does sufficient evidence exist"; these tables track "ask a
-- specific party for it, by when, and record what came back," without
-- conflating receipt with acceptance.

CREATE TABLE evidence_requests (
    request_id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    legal_entity_id UUID NOT NULL,
    requirement_id UUID REFERENCES evidence_requirements(evidence_requirement_id),
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    assigned_to_principal_id VARCHAR(255) NOT NULL,
    due_at TIMESTAMPTZ NOT NULL,
    status VARCHAR(25) NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT','SENT','VIEWED','RESPONSE_RECEIVED','UNDER_EVALUATION','SATISFIED','CLARIFICATION_REQUIRED','CLOSED')),
    created_by_principal_id VARCHAR(255) NOT NULL,
    correlation_id VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_evidence_requests_tenant ON evidence_requests (tenant_id, legal_entity_id);
CREATE UNIQUE INDEX evidence_request_create_idempotency_unique ON evidence_requests (tenant_id, correlation_id);

CREATE TABLE evidence_request_responses (
    response_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id UUID NOT NULL REFERENCES evidence_requests(request_id),
    tenant_id UUID NOT NULL,
    version INTEGER NOT NULL,
    submitted_by_principal_id VARCHAR(255) NOT NULL,
    artifact_document_id UUID,
    malware_scan_status VARCHAR(15) NOT NULL DEFAULT 'PENDING'
        CHECK (malware_scan_status IN ('PENDING','CLEAN','QUARANTINED')),
    correlation_id VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (request_id, version)
);
CREATE UNIQUE INDEX evidence_response_idempotency_unique ON evidence_request_responses (tenant_id, correlation_id);

CREATE TABLE evidence_request_notes (
    note_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id UUID NOT NULL REFERENCES evidence_requests(request_id),
    tenant_id UUID NOT NULL,
    author_principal_id VARCHAR(255) NOT NULL,
    body TEXT NOT NULL,
    visibility VARCHAR(15) NOT NULL CHECK (visibility IN ('SHARED','AUDIT_ONLY')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE evidence_request_receipts (
    receipt_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    response_id UUID NOT NULL REFERENCES evidence_request_responses(response_id),
    tenant_id UUID NOT NULL,
    acknowledged_by_principal_id VARCHAR(255) NOT NULL,
    acknowledged_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE evidence_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_requests
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE evidence_request_responses ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_request_responses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_request_responses
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE evidence_request_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_request_notes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_request_notes
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE evidence_request_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_request_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_request_receipts
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- "New PBC response overwrites prior version" (AUD-NEG-017): a response,
-- once recorded, is permanent — SubmitResponse always INSERTs the next
-- version rather than touching an existing row.
-- Only malware_scan_status may ever change on a response row (the
-- ScanResult callback's own write) — every other column, once submitted,
-- is permanent.
CREATE OR REPLACE FUNCTION reject_evidence_response_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evidence request responses are never deleted';
    END IF;
    IF NEW.request_id IS DISTINCT FROM OLD.request_id
        OR NEW.version IS DISTINCT FROM OLD.version
        OR NEW.submitted_by_principal_id IS DISTINCT FROM OLD.submitted_by_principal_id
        OR NEW.artifact_document_id IS DISTINCT FROM OLD.artifact_document_id THEN
        RAISE EXCEPTION 'evidence request response content is immutable — only malware_scan_status may be updated';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_evidence_response_mutation
    BEFORE UPDATE OR DELETE ON evidence_request_responses
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_response_mutation();

CREATE OR REPLACE FUNCTION reject_evidence_note_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'evidence request notes are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_evidence_note_mutation
    BEFORE UPDATE OR DELETE ON evidence_request_notes
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_note_mutation();

CREATE OR REPLACE FUNCTION reject_evidence_receipt_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'evidence request receipts are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_evidence_receipt_mutation
    BEFORE UPDATE OR DELETE ON evidence_request_receipts
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_receipt_mutation();
