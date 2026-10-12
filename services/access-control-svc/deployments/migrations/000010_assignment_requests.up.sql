-- Migration 000010: governed access-assignment requests (Authorization Standard
-- §9 "Assignment request", §21 POST /v1/iam/access-assignments).
--
-- authorization-svc holds the access assignment itself (principal_role_
-- assignments) and enforces it. What it does not have is a GOVERNED way to
-- create one: its admin API assigns on the spot for anyone holding
-- iam.assignment.grant. §9 asks for a "governed request to create/modify
-- assignment" that "may require manager/data-owner/security approval depending
-- on risk". This table is that request, and the lifecycle around it:
--
--   PENDING_APPROVAL -> PROVISIONED      approved by an independent principal
--   PENDING_APPROVAL -> REJECTED | CANCELLED
--   (STANDARD risk, not a self-request)  -> PROVISIONED directly
--   PROVISIONED      -> REVOKED          §21 POST .../{id}:revoke
--
-- A request is provisioned into authorization-svc BEFORE its row says so, and
-- the row records authorization-svc's assignment id, so a PROVISIONED row
-- always names an assignment that exists there.

CREATE TABLE IF NOT EXISTS assignment_requests (
    request_id               UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    target_principal_id      VARCHAR(255) NOT NULL,
    role_definition_id       UUID NOT NULL REFERENCES role_definitions(role_definition_id),
    legal_entity_id          VARCHAR(255) NOT NULL,
    effective_from           TIMESTAMPTZ  NOT NULL,
    justification            TEXT         NOT NULL,
    -- Computed by the service from the role's actions (000008 risk tiers,
    -- protected actions) and from who is asking; never taken from the caller.
    risk_tier                VARCHAR(10)  NOT NULL CHECK (risk_tier IN ('STANDARD', 'HIGH', 'CRITICAL')),
    approval_required        BOOLEAN      NOT NULL,
    approval_reason          TEXT         NOT NULL DEFAULT '',
    status                   VARCHAR(20)  NOT NULL CHECK (status IN
                                 ('PENDING_APPROVAL', 'PROVISIONED', 'REJECTED', 'CANCELLED', 'REVOKED')),
    requested_by_principal_id VARCHAR(255) NOT NULL,
    decided_by_principal_id  VARCHAR(255),
    decision_reason          TEXT,
    decided_at               TIMESTAMPTZ,
    authz_assignment_id      UUID,
    revoked_by_principal_id  VARCHAR(255),
    revocation_reason        TEXT,
    revoked_at               TIMESTAMPTZ,
    correlation_id           VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- A PROVISIONED or REVOKED request names the assignment it created.
    CONSTRAINT assignment_requests_provisioned_has_assignment CHECK (
        status NOT IN ('PROVISIONED', 'REVOKED') OR authz_assignment_id IS NOT NULL
    ),
    -- Nobody approves their own request or their own access (§10.1 row 6,
    -- "no self-grant of protected privilege"; §10.2 "own access elevation").
    CONSTRAINT assignment_requests_independent_decision CHECK (
        decided_by_principal_id IS NULL
        OR status = 'CANCELLED'
        OR (decided_by_principal_id <> target_principal_id
            AND (NOT approval_required OR decided_by_principal_id <> requested_by_principal_id))
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_assignment_requests_tenant_correlation
    ON assignment_requests (tenant_id, correlation_id);
CREATE INDEX IF NOT EXISTS idx_assignment_requests_tenant_status
    ON assignment_requests (tenant_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_assignment_requests_target
    ON assignment_requests (tenant_id, target_principal_id);
CREATE INDEX IF NOT EXISTS idx_assignment_requests_authz_assignment
    ON assignment_requests (tenant_id, authz_assignment_id) WHERE authz_assignment_id IS NOT NULL;

ALTER TABLE assignment_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE assignment_requests FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS assignment_requests_tenant ON assignment_requests;
CREATE POLICY assignment_requests_tenant ON assignment_requests FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- The events the governance surface emits (§23). The outbox CHECK names every
-- event type this service may enqueue, so an unknown type fails at write time
-- rather than reaching consumers.
ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known CHECK (event_type IN (
    'role.created', 'role.updated', 'permission.bundle.updated',
    'iam.role.published',
    'iam.assignment.requested', 'iam.assignment.granted', 'iam.assignment.revoked',
    'iam.access_review.started', 'iam.access_review.completed'
));
