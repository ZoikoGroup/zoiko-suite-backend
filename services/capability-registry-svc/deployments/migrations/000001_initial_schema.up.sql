-- 000001_initial_schema.up.sql
-- capability-registry-svc — Chunk 7 (doc7 §7 "Capability, Module & Release
-- Registry"). Five tables answering five independent questions; see the
-- domain package doc comment for the full mapping.

CREATE TABLE capabilities (
    capability_id           UUID PRIMARY KEY,
    capability_code         VARCHAR(128) NOT NULL,
    module_domain           VARCHAR(128) NOT NULL,
    version                 INT NOT NULL DEFAULT 1,
    dependencies            TEXT,
    execution_risk_class    VARCHAR(32) NOT NULL,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL
);

CREATE UNIQUE INDEX idx_capabilities_code_unique ON capabilities (capability_code);

CREATE TABLE market_releases (
    market_release_id       UUID PRIMARY KEY,
    capability_id           UUID NOT NULL REFERENCES capabilities(capability_id),
    market_code             VARCHAR(16) NOT NULL,
    language_code           VARCHAR(16),
    legal_approval_status   VARCHAR(32) NOT NULL,
    state                   VARCHAR(32) NOT NULL,
    effective_from          TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to            TIMESTAMP WITH TIME ZONE,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL
);

CREATE INDEX idx_market_releases_capability_market ON market_releases (capability_id, market_code);

CREATE TABLE integration_capabilities (
    integration_capability_id UUID PRIMARY KEY,
    capability_id             UUID NOT NULL REFERENCES capabilities(capability_id),
    provider_code              VARCHAR(128) NOT NULL,
    certified                   BOOLEAN NOT NULL DEFAULT FALSE,
    health_status                VARCHAR(32) NOT NULL DEFAULT 'UNKNOWN',
    created_at                   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at                   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id      VARCHAR(255) NOT NULL
);

CREATE UNIQUE INDEX idx_integration_capabilities_unique ON integration_capabilities (capability_id, provider_code);

-- releases: current operational state (INCIDENT_RESTRICTED/DISABLED etc.).
-- Only the current row per capability matters for resolution, but history
-- is kept append-only (no UPDATE, always INSERT a new row) so an incident
-- restriction and its later lift are both auditable events, per doc7 §32.1
-- kill-switch doctrine ("does not erase already-committed history").
CREATE TABLE releases (
    release_id              UUID PRIMARY KEY,
    capability_id            UUID NOT NULL REFERENCES capabilities(capability_id),
    state                    VARCHAR(32) NOT NULL,
    reason                   TEXT,
    effective_from           TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL
);

CREATE INDEX idx_releases_capability_effective ON releases (capability_id, effective_from DESC);

CREATE TABLE capability_claims (
    claim_id                    UUID PRIMARY KEY,
    capability_id                UUID NOT NULL REFERENCES capabilities(capability_id),
    claim_text                    TEXT NOT NULL,
    market_scope                  VARCHAR(255),
    wording_owner_principal_id     VARCHAR(255) NOT NULL,
    approved_by_principal_id       VARCHAR(255) NOT NULL,
    expiry_review_date              TIMESTAMP WITH TIME ZONE,
    created_at                      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id         VARCHAR(255) NOT NULL
);

CREATE INDEX idx_capability_claims_capability ON capability_claims (capability_id);
