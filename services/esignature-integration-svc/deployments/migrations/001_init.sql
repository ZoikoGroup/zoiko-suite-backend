CREATE TABLE IF NOT EXISTS signature_envelopes (
    envelope_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    provider VARCHAR(64) NOT NULL,
    document_title VARCHAR(512) NOT NULL,
    signer_email VARCHAR(256) NOT NULL,
    signer_name VARCHAR(256) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'SENT',
    external_ref VARCHAR(256),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sig_envelopes_tenant_entity ON signature_envelopes(tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_sig_envelopes_status ON signature_envelopes(status);
