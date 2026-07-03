-- Migration: 000001_initial_schema.up.sql
--
-- Owned records for identity-context-svc per docs/architecture/04-data-model.md
-- §06.1: Principal, PrincipalRoleAssignment, DelegatedAuthority.
--
-- ID columns are VARCHAR, not UUID: principal_id and related identifiers are
-- ULIDs (github.com/oklog/ulid), which are not valid Postgres UUID literals.
--
-- No RLS on these tables yet: PrincipalStore's read methods (FindByID,
-- FindActiveRoleAssignments, FindActiveDelegations, UpdateStatus) do not
-- carry tenant_id through the interface today, so there is no tenant value
-- to enforce a session-level RLS policy against on those paths. FindByIDPSubject
-- filters by tenant_id at the query level instead. Tracked as a follow-up:
-- thread tenant_id through PrincipalStore so RLS can match the
-- tenant-entity-registry-svc pattern.
--
-- Migrations are run via golang-migrate CLI in CI/CD. Do NOT auto-run on
-- service startup.

CREATE TABLE principals (
    principal_id                VARCHAR(255) PRIMARY KEY,
    tenant_id                   VARCHAR(255) NOT NULL,
    principal_type              VARCHAR(50)  NOT NULL,
    identity_provider_subject   VARCHAR(500) NOT NULL,
    email                       VARCHAR(320) NOT NULL,
    display_name                VARCHAR(255) NOT NULL,
    status                      VARCHAR(50)  NOT NULL,
    created_at                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, identity_provider_subject)
);

CREATE INDEX idx_principals_tenant ON principals (tenant_id);

CREATE TABLE principal_role_assignments (
    assignment_id     VARCHAR(255) PRIMARY KEY,
    principal_id      VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    tenant_id         VARCHAR(255) NOT NULL,
    role_id           VARCHAR(255) NOT NULL,
    legal_entity_id   VARCHAR(255),
    effective_from    TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to      TIMESTAMP WITH TIME ZONE NOT NULL,
    assigned_by       VARCHAR(255) NOT NULL,
    created_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_role_assignments_principal_window
    ON principal_role_assignments (principal_id, effective_from, effective_to);

CREATE TABLE delegated_authorities (
    delegated_authority_id  VARCHAR(255) PRIMARY KEY,
    delegator_principal_id  VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    delegate_principal_id   VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    tenant_id                VARCHAR(255) NOT NULL,
    scope_type               VARCHAR(50)  NOT NULL,
    legal_entity_id          VARCHAR(255),
    authority_limit_type     VARCHAR(50)  NOT NULL,
    authority_limit_value    NUMERIC      NOT NULL,
    effective_from           TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to             TIMESTAMP WITH TIME ZONE NOT NULL,
    revocation_status        VARCHAR(50)  NOT NULL,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_delegations_delegate_window
    ON delegated_authorities (delegate_principal_id, revocation_status, effective_from, effective_to);

-- Evidence obligation: every principal status transition is append-only
-- audit trail (doctrine §2.11 — no DELETE, no overwrite of history).
CREATE TABLE access_decision_log (
    decision_log_id    VARCHAR(255) PRIMARY KEY,
    principal_id       VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    tenant_id          VARCHAR(255) NOT NULL,
    action_type        VARCHAR(100) NOT NULL,
    decision_outcome   VARCHAR(50)  NOT NULL,
    decision_basis     VARCHAR(255) NOT NULL,
    correlation_id     VARCHAR(255) NOT NULL,
    decided_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_decision_log_principal ON access_decision_log (principal_id);
