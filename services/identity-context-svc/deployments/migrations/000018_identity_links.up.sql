-- Migration: 000018_identity_links.up.sql
--
-- IC-3: durable identity links and subject-merge tracking.
--
-- identity_links binds an external IdP subject (OIDC sub, SAML NameID) to a
-- local principal within a tenant. Backs GET /v1/subjects/{id}/links and the
-- merge path: when two duplicate subjects are merged, every link that named
-- the retired principal is repointed at the survivor, and the retired
-- principal is disabled so its ID is never reissued.
--
-- identity_merges records each completed merge for audit: which survivor
-- absorbed which retired principal, who approved it (step-up required), and
-- when.
--
-- ID columns are ULIDs (VARCHAR), matching 000001.

CREATE TABLE identity_links (
    link_id            VARCHAR(255) PRIMARY KEY,
    tenant_id          VARCHAR(255) NOT NULL,
    principal_id       VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    identity_provider  VARCHAR(100) NOT NULL,
    provider_subject   VARCHAR(255) NOT NULL,
    provider_type      VARCHAR(20)  NOT NULL DEFAULT 'OIDC',
    created_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    CONSTRAINT identity_links_provider_type_check
        CHECK (provider_type IN ('OIDC','SAML')),

    CONSTRAINT identity_links_one_link_per_provider_subject
        UNIQUE (tenant_id, identity_provider, provider_subject)
);

CREATE INDEX idx_identity_links_principal
    ON identity_links (principal_id);

CREATE INDEX idx_identity_links_tenant_provider_subject
    ON identity_links (tenant_id, identity_provider, provider_subject);

CREATE TABLE identity_merges (
    merge_id         VARCHAR(255) PRIMARY KEY,
    tenant_id        VARCHAR(255) NOT NULL,
    survivor_id      VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    retired_id       VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    approved_by      VARCHAR(255) NOT NULL,
    approved_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    step_up_evidence VARCHAR(255) NOT NULL,

    CONSTRAINT identity_merges_survivor_not_retired
        CHECK (survivor_id <> retired_id)
);

CREATE INDEX idx_identity_merges_tenant
    ON identity_merges (tenant_id, approved_at DESC);

ALTER TABLE identity_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON identity_links
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE identity_merges ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity_merges FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON identity_merges
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Grants are guarded on the role, as 000014 and 000019 are, so a database
-- without create-app-roles.sh applied (CI, the store tests) still migrates.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_identity_context') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON identity_links TO app_identity_context;
        GRANT SELECT, INSERT ON identity_merges TO app_identity_context;
    END IF;
END
$$;
