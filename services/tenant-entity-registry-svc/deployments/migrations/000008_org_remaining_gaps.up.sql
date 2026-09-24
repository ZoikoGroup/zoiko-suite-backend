-- 000008_org_remaining_gaps.up.sql
--
-- The remaining ORG-02 / ORG-03 gaps from the 23 Sep 2026 Group 1 audit:
--
--   1. Onboarding idempotency and evidence (§4.2 "Create by approved onboarding
--      correlation/external customer key"; evidence "onboarding request").
--   2. FailedProvisioning (§4.2 "Provisioning partial failure remains
--      Provisioning/FailedProvisioning with compensating cleanup").
--   3. LEI (ORG-03 mandatory control: "Where LEI is available, store LEI as an
--      external organizational identifier with source/status").
--   4. Draft → Verified → Active (§4.3 lifecycle).
--   5. MergeDuplicateCandidate, non-destructively (§4.3 named command, §1
--      "destructive merge is prohibited", "merge/split SHALL preserve lineage").

-- ---------------------------------------------------------------------------
-- 1. Onboarding key and evidence
-- ---------------------------------------------------------------------------

ALTER TABLE tenants
    ADD COLUMN external_customer_key  VARCHAR(255),
    ADD COLUMN onboarding_request_ref VARCHAR(255),
    ADD COLUMN provisioning_failure_reason TEXT,
    ADD COLUMN provisioning_failed_at TIMESTAMP WITH TIME ZONE;

-- The replay lookup. Deliberately WITHOUT row-level security, for the same
-- reason as tenant_host_bindings: it is read BEFORE a tenant is known — a
-- retried onboarding does not know the tenant id it is asking about, which is
-- the whole point of the key. It holds no tenant data: a key, the tenant id it
-- produced and a hash of the request, so knowing a row grants nothing.
--
-- Written in the same transaction as the tenant and FIRST, so a replay fails
-- on this primary key rather than on tenant_code — the difference between
-- "you already did this, here is the tenant" and "that code is taken". The FK
-- is DEFERRABLE because the tenant row does not exist yet at that point.
CREATE TABLE tenant_onboarding_keys (
    external_customer_key VARCHAR(255) PRIMARY KEY,
    tenant_id             UUID NOT NULL UNIQUE
        REFERENCES tenants(tenant_id) DEFERRABLE INITIALLY DEFERRED,
    request_fingerprint   CHAR(64) NOT NULL,
    created_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX tenants_external_customer_key_uq
    ON tenants (external_customer_key) WHERE external_customer_key IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 3. LEI on the effective-dated profile
-- ---------------------------------------------------------------------------
--
-- On the version, not the entity: an LEI is issued, lapses, is retired or
-- transferred, and "what was this entity's LEI and its status on 31 March" is
-- an as-of question like any other identity field.

ALTER TABLE legal_entity_profile_versions
    ADD COLUMN lei             CHAR(20),
    ADD COLUMN lei_source      VARCHAR(64),
    ADD COLUMN lei_status      VARCHAR(32),
    ADD COLUMN lei_verified_at TIMESTAMP WITH TIME ZONE;

ALTER TABLE legal_entity_profile_versions
    -- ISO 17442: 18 alphanumerics then 2 check digits. The mod-97 checksum is
    -- enforced by the service; the shape is enforced here too.
    ADD CONSTRAINT lepv_lei_format CHECK (lei IS NULL OR lei ~ '^[A-Z0-9]{18}[0-9]{2}$'),
    -- "with source/status": an LEI without them is exactly the bare
    -- identifier the control exists to prevent.
    ADD CONSTRAINT lepv_lei_has_source_and_status CHECK (
        lei IS NULL OR (lei_source IS NOT NULL AND lei_status IS NOT NULL)),
    -- GLEIF registration statuses.
    ADD CONSTRAINT lepv_lei_status_known CHECK (lei_status IS NULL OR lei_status IN (
        'ISSUED', 'LAPSED', 'PENDING_TRANSFER', 'PENDING_ARCHIVAL', 'MERGED',
        'RETIRED', 'ANNULLED', 'DUPLICATE', 'TRANSFERRED', 'CANCELLED'));

-- ---------------------------------------------------------------------------
-- 4. Verification (Draft → Verified → Active)
-- ---------------------------------------------------------------------------

ALTER TABLE legal_entities
    ADD COLUMN verified_by_principal_id         VARCHAR(255),
    ADD COLUMN verified_at                      TIMESTAMP WITH TIME ZONE,
    ADD COLUMN verification_evidence_ref        VARCHAR(255),
    ADD COLUMN verification_approval_request_id UUID REFERENCES approval_requests(approval_request_id),
    ADD COLUMN merged_into_legal_entity_id      UUID REFERENCES legal_entities(legal_entity_id),
    ADD COLUMN merged_at                        TIMESTAMP WITH TIME ZONE;

ALTER TABLE legal_entities
    -- "Material changes independently approved": the principal who created
    -- an entity may not be the one who verifies it. NOT VALID so history
    -- written before this migration does not fail it.
    ADD CONSTRAINT le_no_self_verification CHECK (
        verified_by_principal_id IS NULL
        OR verified_by_principal_id <> created_by_principal_id) NOT VALID,
    ADD CONSTRAINT le_not_merged_into_self CHECK (
        merged_into_legal_entity_id IS NULL
        OR merged_into_legal_entity_id <> legal_entity_id);

-- ---------------------------------------------------------------------------
-- 5. Non-destructive merge lineage
-- ---------------------------------------------------------------------------
--
-- A merge here deletes nothing and re-points nothing. The duplicate becomes
-- DORMANT and records which entity it was merged into; this table is the
-- lineage — who proposed, who approved, and, if it was reversed, who reversed
-- it and who approved that. Rows are never deleted.

CREATE TABLE entity_merge_records (
    merge_record_id              UUID PRIMARY KEY,
    tenant_id                    UUID NOT NULL REFERENCES tenants(tenant_id),
    duplicate_legal_entity_id    UUID NOT NULL REFERENCES legal_entities(legal_entity_id),
    survivor_legal_entity_id     UUID NOT NULL REFERENCES legal_entities(legal_entity_id),
    prior_entity_status          VARCHAR(50) NOT NULL,
    reason                       TEXT NOT NULL,
    evidence_ref                 VARCHAR(255),

    merged_by_principal_id       VARCHAR(255) NOT NULL,
    merge_approved_by_principal_id VARCHAR(255) NOT NULL,
    merge_approval_request_id    UUID NOT NULL REFERENCES approval_requests(approval_request_id),
    merged_at                    TIMESTAMP WITH TIME ZONE NOT NULL,

    unmerged_by_principal_id     VARCHAR(255),
    unmerge_approved_by_principal_id VARCHAR(255),
    unmerge_approval_request_id  UUID REFERENCES approval_requests(approval_request_id),
    unmerged_at                  TIMESTAMP WITH TIME ZONE,
    unmerge_reason               TEXT,

    CONSTRAINT emr_distinct_entities CHECK (duplicate_legal_entity_id <> survivor_legal_entity_id),
    -- §4.3 "no self-approval of merge", in the schema.
    CONSTRAINT emr_no_self_approval CHECK (merge_approved_by_principal_id <> merged_by_principal_id),
    CONSTRAINT emr_no_self_unmerge_approval CHECK (
        unmerge_approved_by_principal_id IS NULL
        OR unmerge_approved_by_principal_id <> unmerged_by_principal_id),
    CONSTRAINT emr_unmerge_complete CHECK (
        (unmerged_at IS NULL AND unmerged_by_principal_id IS NULL AND unmerge_approved_by_principal_id IS NULL)
        OR
        (unmerged_at IS NOT NULL AND unmerged_by_principal_id IS NOT NULL AND unmerge_approved_by_principal_id IS NOT NULL))
);

-- At most one live merge per duplicate.
CREATE UNIQUE INDEX emr_one_live_merge
    ON entity_merge_records (duplicate_legal_entity_id) WHERE unmerged_at IS NULL;

ALTER TABLE entity_merge_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_merge_records FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON entity_merge_records
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ---------------------------------------------------------------------------
-- Approval subjects for the new governed commands
-- ---------------------------------------------------------------------------

ALTER TABLE approval_requests DROP CONSTRAINT ar_subject_known;
ALTER TABLE approval_requests ADD CONSTRAINT ar_subject_known CHECK (subject_type IN (
    'TENANT_CREATION', 'TENANT_COMMAND',
    'LEGAL_PROFILE_AMENDMENT', 'REGISTRY_CONFLICT_RESOLUTION',
    'LEGAL_ENTITY_VERIFICATION', 'LEGAL_ENTITY_MERGE', 'LEGAL_ENTITY_UNMERGE'));
