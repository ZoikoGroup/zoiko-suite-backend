-- Migration 000014: group subjects (Authorization Standard §2, §9, §22, A20).
--
-- §9 defines the access assignment as "subject/GROUP + role/policy + scope +
-- effective dates", and §2 the Group as an "administrative collection of
-- subjects; may be SCIM-provisioned", whose membership "may create candidate
-- assignments but never bypass policy". §22 places group / group_membership
-- with "Identity/IAM" — this service is the IAM governance owner, and no
-- service in the estate held groups (Group 1 audit gap S9-C1, the one partial
-- row of this service's §9 table).
--
-- A group assignment is not enforced as a group: authorization-svc evaluates
-- principals. It fans out to ONE governed assignment request per member
-- (assignment_requests.group_assignment_id), so every member grant passes the
-- same SoD check, risk-tiered approval and provisioning an individual request
-- does — a protected role reached through a group "enters the required
-- approval path" (A20) per member, and nothing about membership bypasses it.

CREATE TABLE IF NOT EXISTS iam_groups (
    group_id               UUID PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    -- The entity whose ROLE_MANAGE holders administer the group.
    legal_entity_id        VARCHAR(255) NOT NULL,
    group_code             VARCHAR(100) NOT NULL,
    group_name             VARCHAR(255) NOT NULL,
    status                 VARCHAR(10)  NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'RETIRED')),
    -- Provenance: MANUAL, or SCIM when an external directory provisions it.
    source                 VARCHAR(10)  NOT NULL DEFAULT 'MANUAL' CHECK (source IN ('MANUAL', 'SCIM')),
    created_by_principal_id VARCHAR(255) NOT NULL,
    correlation_id         VARCHAR(255) NOT NULL,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, group_code),
    UNIQUE (tenant_id, correlation_id)
);

CREATE TABLE IF NOT EXISTS iam_group_members (
    group_id               UUID         NOT NULL REFERENCES iam_groups (group_id),
    tenant_id              VARCHAR(255) NOT NULL,
    principal_id           VARCHAR(255) NOT NULL,
    added_by_principal_id  VARCHAR(255) NOT NULL,
    added_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    removed_by_principal_id VARCHAR(255),
    removed_at             TIMESTAMPTZ,
    removal_reason         TEXT,
    CHECK (removed_at IS NULL OR removed_by_principal_id IS NOT NULL)
);
-- One live membership per principal per group; history is kept.
CREATE UNIQUE INDEX IF NOT EXISTS idx_iam_group_members_live
    ON iam_group_members (group_id, principal_id) WHERE removed_at IS NULL;

CREATE TABLE IF NOT EXISTS iam_group_assignments (
    group_assignment_id    UUID PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    group_id               UUID         NOT NULL REFERENCES iam_groups (group_id),
    role_definition_id     UUID         NOT NULL REFERENCES role_definitions (role_definition_id),
    legal_entity_id        VARCHAR(255) NOT NULL,
    effective_from         TIMESTAMPTZ  NOT NULL,
    effective_to           TIMESTAMPTZ,
    justification          TEXT         NOT NULL,
    status                 VARCHAR(10)  NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'REVOKED')),
    created_by_principal_id VARCHAR(255) NOT NULL,
    revoked_by_principal_id VARCHAR(255),
    revocation_reason      TEXT,
    revoked_at             TIMESTAMPTZ,
    correlation_id         VARCHAR(255) NOT NULL,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, correlation_id),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);

ALTER TABLE assignment_requests
    ADD COLUMN IF NOT EXISTS group_assignment_id UUID REFERENCES iam_group_assignments (group_assignment_id);
CREATE INDEX IF NOT EXISTS idx_assignment_requests_group
    ON assignment_requests (tenant_id, group_assignment_id, target_principal_id) WHERE group_assignment_id IS NOT NULL;

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['iam_groups', 'iam_group_members', 'iam_group_assignments'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))$p$, t);
    END LOOP;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'zoiko_app') THEN
        GRANT SELECT, INSERT, UPDATE ON iam_groups, iam_group_members, iam_group_assignments TO zoiko_app;
    END IF;
END
$$;
