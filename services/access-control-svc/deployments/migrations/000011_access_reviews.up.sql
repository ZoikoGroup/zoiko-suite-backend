-- Migration 000011: access review / attestation campaigns (Authorization
-- Standard §9 "Assignment review", §24 "Access Reviews & Continuous
-- Governance").
--
-- authorization-svc has an access_reviews table (its 000017) and a decide
-- route, but nothing in the estate creates a review: no campaign could ever be
-- started, so §9's "stale/unowned assignments must be removable" had no
-- mechanism. A campaign here snapshots the live assignments of the roles it
-- covers from authorization-svc, flags orphans (§24 "assignments without valid
-- ... resource scope") and toxic combinations (§24 "newly introduced SoD
-- conflicts"), and a REVOKE decision revokes the assignment there.
--
-- Closing rules (§24 "unresolved high-risk review cannot silently close"):
-- a campaign refuses to complete while any HIGH/CRITICAL item is undecided or
-- escalated; a STANDARD item left open at completion is recorded as EXPIRED,
-- and the completion event counts them.

CREATE TABLE IF NOT EXISTS access_review_campaigns (
    campaign_id              UUID PRIMARY KEY,
    tenant_id                VARCHAR(255) NOT NULL,
    campaign_name            VARCHAR(255) NOT NULL,
    review_type              VARCHAR(20)  NOT NULL CHECK (review_type IN ('PERIODIC', 'EVENT_TRIGGERED', 'PRIVILEGED')),
    trigger_reason           TEXT         NOT NULL DEFAULT '',
    legal_entity_id          VARCHAR(255) NOT NULL,
    default_reviewer_principal_id VARCHAR(255) NOT NULL,
    status                   VARCHAR(20)  NOT NULL CHECK (status IN ('OPEN', 'COMPLETED')),
    due_at                   TIMESTAMPTZ  NOT NULL,
    created_by_principal_id  VARCHAR(255) NOT NULL,
    completed_by_principal_id VARCHAR(255),
    completed_at             TIMESTAMPTZ,
    correlation_id           VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_access_review_campaigns_tenant_correlation
    ON access_review_campaigns (tenant_id, correlation_id);
CREATE INDEX IF NOT EXISTS idx_access_review_campaigns_tenant_status
    ON access_review_campaigns (tenant_id, status, created_at DESC);

CREATE TABLE IF NOT EXISTS access_review_items (
    item_id                  UUID PRIMARY KEY,
    campaign_id              UUID NOT NULL REFERENCES access_review_campaigns(campaign_id),
    tenant_id                VARCHAR(255) NOT NULL,
    authz_assignment_id      UUID NOT NULL,
    target_principal_id      VARCHAR(255) NOT NULL,
    role_definition_id       UUID NOT NULL,
    role_code                VARCHAR(100) NOT NULL,
    legal_entity_id          VARCHAR(255),
    granted_actions          TEXT[]       NOT NULL DEFAULT '{}',
    risk_tier                VARCHAR(10)  NOT NULL CHECK (risk_tier IN ('STANDARD', 'HIGH', 'CRITICAL')),
    -- ORPHANED_ROLE (the role is retired here but still assigned there),
    -- SOD_CONFLICT (the principal's reviewed roles combine to a §10.1 conflict),
    -- SELF_REVIEW_REASSIGNED (the default reviewer is the subject).
    flags                    TEXT[]       NOT NULL DEFAULT '{}',
    reviewer_principal_id    VARCHAR(255) NOT NULL,
    status                   VARCHAR(20)  NOT NULL CHECK (status IN ('OPEN', 'DECIDED', 'ESCALATED', 'EXPIRED')),
    decision                 VARCHAR(20)  CHECK (decision IN ('KEEP', 'REVOKE', 'MODIFY', 'ESCALATE')),
    decision_reason          TEXT,
    decided_by_principal_id  VARCHAR(255),
    decided_at               TIMESTAMPTZ,
    revocation_applied       BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (campaign_id, authz_assignment_id),
    -- No self-attestation: the subject of an item never decides it.
    CONSTRAINT access_review_items_no_self_attestation CHECK (
        decided_by_principal_id IS NULL OR decided_by_principal_id <> target_principal_id
    ),
    CONSTRAINT access_review_items_decided_has_reason CHECK (
        status NOT IN ('DECIDED', 'ESCALATED') OR (decision IS NOT NULL AND decision_reason IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_access_review_items_campaign ON access_review_items (tenant_id, campaign_id);
CREATE INDEX IF NOT EXISTS idx_access_review_items_reviewer ON access_review_items (tenant_id, reviewer_principal_id, status);

ALTER TABLE access_review_campaigns ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_review_campaigns FORCE ROW LEVEL SECURITY;
ALTER TABLE access_review_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE access_review_items FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS access_review_campaigns_tenant ON access_review_campaigns;
CREATE POLICY access_review_campaigns_tenant ON access_review_campaigns FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
DROP POLICY IF EXISTS access_review_items_tenant ON access_review_items;
CREATE POLICY access_review_items_tenant ON access_review_items FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
