-- 000007_approval_requests.up.sql
--
-- Verified maker-checker for ORG-02 §4.2 and ORG-03 §4.3.
--
-- Until this migration every "independently approved" control in this service
-- was satisfied by the maker typing a second principal id into their own
-- request body: approved_by_principal_id was never checked against anything
-- but string inequality with the actor, in the service and in the CHECKs. One
-- person could terminate a tenant or rename a legal entity alone.
--
-- The fix is a two-step flow. A maker-checker command no longer executes; it
-- files an approval_requests row and answers 202. A DIFFERENT principal, whose
-- identity is the gateway-verified caller of /approve — never a body field —
-- then approves it, and only then does the stored command run. The approver
-- column below is therefore written exclusively from verified identity.
--
-- payload_fingerprint is §3's "activation/approval binds protected-field
-- fingerprint": the approver must present the fingerprint of what they
-- reviewed, and the service recomputes it from the stored payload, so an
-- approval cannot be applied to anything other than exactly what was proposed.

CREATE TABLE approval_requests (
    approval_request_id       UUID PRIMARY KEY,
    tenant_id                 UUID NOT NULL REFERENCES tenants(tenant_id),

    subject_type              VARCHAR(40) NOT NULL,
    -- The tenant, legal entity or registry conflict the command acts on.
    subject_id                UUID NOT NULL,
    -- The named command — InitiateTermination, ChangeLegalName, CreateTenant …
    command_name              VARCHAR(64) NOT NULL,
    -- The command exactly as proposed. Executed as stored on approval.
    payload                   JSONB NOT NULL,
    -- The subject's record_version at proposal. 0 where the subject has none
    -- (registry conflicts are guarded by status instead).
    expected_version          BIGINT NOT NULL,
    payload_fingerprint       CHAR(64) NOT NULL,
    reason                    TEXT NOT NULL,

    requested_by_principal_id VARCHAR(255) NOT NULL,
    requested_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    expires_at                TIMESTAMP WITH TIME ZONE NOT NULL,

    status                    VARCHAR(16) NOT NULL DEFAULT 'PENDING',
    decided_by_principal_id   VARCHAR(255),
    decided_at                TIMESTAMP WITH TIME ZONE,
    decision_note             TEXT,
    correlation_id            VARCHAR(255),

    CONSTRAINT ar_subject_known CHECK (subject_type IN (
        'TENANT_CREATION', 'TENANT_COMMAND',
        'LEGAL_PROFILE_AMENDMENT', 'REGISTRY_CONFLICT_RESOLUTION')),
    CONSTRAINT ar_status_known CHECK (status IN (
        'PENDING', 'APPROVED', 'REJECTED', 'STALE', 'EXPIRED')),
    -- The control itself, enforced independently of the service.
    CONSTRAINT ar_no_self_decision CHECK (
        decided_by_principal_id IS NULL
        OR decided_by_principal_id <> requested_by_principal_id),
    -- A decided request says when; a pending one does not pretend to be.
    CONSTRAINT ar_decided_iff_not_pending CHECK (
        (status = 'PENDING') = (decided_at IS NULL)),
    -- APPROVED and REJECTED are human decisions and must name the human.
    -- STALE and EXPIRED are the service's conclusions and name nobody.
    CONSTRAINT ar_decision_has_decider CHECK (
        status NOT IN ('APPROVED', 'REJECTED') OR decided_by_principal_id IS NOT NULL),
    CONSTRAINT ar_expiry_after_request CHECK (expires_at > requested_at)
);

-- At most one open proposal per subject. A second maker cannot queue a
-- competing command behind the first; one of them could only ever go stale.
CREATE UNIQUE INDEX ar_one_pending_per_subject
    ON approval_requests (subject_type, subject_id)
    WHERE status = 'PENDING';

CREATE INDEX idx_ar_tenant_pending
    ON approval_requests (tenant_id, requested_at DESC)
    WHERE status = 'PENDING';

CREATE INDEX idx_ar_subject
    ON approval_requests (subject_type, subject_id, requested_at DESC);

ALTER TABLE approval_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON approval_requests
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ---------------------------------------------------------------------------
-- Link each executed fact to the approval that authorised it — the "approval
-- chain" of the ORG-02/ORG-03 mandatory engineering controls.
-- ---------------------------------------------------------------------------

ALTER TABLE tenant_lifecycle_history
    ADD COLUMN approval_request_id UUID REFERENCES approval_requests(approval_request_id);

ALTER TABLE legal_entity_profile_versions
    ADD COLUMN approval_request_id UUID REFERENCES approval_requests(approval_request_id);

-- legal_entity_profile_versions had no self-approval CHECK at all, unlike
-- tenant_lifecycle_history. NOT VALID: enforced for every new row without
-- failing the migration on history written under the old rules.
ALTER TABLE legal_entity_profile_versions
    ADD CONSTRAINT lepv_no_self_approval CHECK (
        approved_by_principal_id IS NULL
        OR approved_by_principal_id <> created_by_principal_id) NOT VALID;

-- ORG-03 "no self-approval of merge": a conflict resolution now carries an
-- approver, who may be neither the resolver who proposed it nor the principal
-- whose claim was quarantined.
ALTER TABLE entity_registry_conflicts
    ADD COLUMN approved_by_principal_id VARCHAR(255),
    ADD COLUMN approval_request_id UUID REFERENCES approval_requests(approval_request_id);

ALTER TABLE entity_registry_conflicts
    ADD CONSTRAINT erc_no_self_approval CHECK (
        approved_by_principal_id IS NULL
        OR (approved_by_principal_id <> resolved_by_principal_id
            AND approved_by_principal_id <> detected_by_principal_id)) NOT VALID;
