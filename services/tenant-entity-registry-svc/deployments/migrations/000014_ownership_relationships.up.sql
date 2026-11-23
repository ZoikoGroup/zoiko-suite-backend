-- 000014_ownership_relationships.up.sql
--
-- ORG-04 Corporate Group & Ownership (ZS-SVC-I-001 s4.4): effective-dated
-- ownership, voting and control relationships between legal entities.
--
-- entity_hierarchies (migration 000001) is a bare parent/child link with no
-- percentages, no evidence and no approval. It stays, so existing callers keep
-- working, but it cannot attest a group structure; this table can.
--
-- Lifecycle: PROPOSED -> ACTIVE -> SUPERSEDED | ENDED, or PROPOSED -> REJECTED.
-- A relationship takes effect only when a principal other than its proposer
-- approves it. That is s4.4's segregation of duties, and the CHECKs below
-- enforce it independently of the service.

-- A relationship may only join two entities of its own tenant. A plain foreign
-- key on legal_entity_id would accept another tenant's entity (FK checks bypass
-- row-level security), so the keys below include tenant_id.
ALTER TABLE legal_entities
    ADD CONSTRAINT le_tenant_entity_unique UNIQUE (tenant_id, legal_entity_id);

CREATE TABLE ownership_relationships (
    relationship_id          UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL REFERENCES tenants(tenant_id),

    parent_legal_entity_id   UUID NOT NULL,
    child_legal_entity_id    UUID NOT NULL,

    -- OWNERSHIP carries an ownership percentage; CONTROL carries a control type
    -- (control without equity is real: board appointment, contract).
    relationship_kind        VARCHAR(16) NOT NULL,
    ownership_pct            NUMERIC(7,4),
    voting_pct               NUMERIC(7,4),
    control_type             VARCHAR(24),
    beneficial_notes         TEXT,

    valid_from               TIMESTAMP WITH TIME ZONE NOT NULL,
    valid_to                 TIMESTAMP WITH TIME ZONE,
    recorded_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    status                   VARCHAR(16) NOT NULL DEFAULT 'PROPOSED',
    -- The filing or contract that evidences the relationship, and why it is
    -- being recorded. Both required: s4.4 "preserve source evidence".
    evidence_ref             TEXT NOT NULL,
    reason                   TEXT NOT NULL,

    proposed_by_principal_id VARCHAR(255) NOT NULL,
    proposed_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    decided_by_principal_id  VARCHAR(255),
    decided_at               TIMESTAMP WITH TIME ZONE,
    decision_note            TEXT,
    correlation_id           VARCHAR(255),

    record_version           BIGINT NOT NULL DEFAULT 1,

    CONSTRAINT or_parent_in_tenant FOREIGN KEY (tenant_id, parent_legal_entity_id)
        REFERENCES legal_entities (tenant_id, legal_entity_id),
    CONSTRAINT or_child_in_tenant FOREIGN KEY (tenant_id, child_legal_entity_id)
        REFERENCES legal_entities (tenant_id, legal_entity_id),
    CONSTRAINT or_kind_known CHECK (relationship_kind IN ('OWNERSHIP', 'CONTROL')),
    CONSTRAINT or_status_known CHECK (status IN
        ('PROPOSED', 'ACTIVE', 'SUPERSEDED', 'ENDED', 'REJECTED')),
    CONSTRAINT or_control_type_known CHECK (control_type IS NULL OR control_type IN
        ('VOTING', 'BOARD', 'CONTRACTUAL', 'OTHER')),
    CONSTRAINT or_no_self_relationship CHECK (parent_legal_entity_id <> child_legal_entity_id),
    CONSTRAINT or_pct_range CHECK (
        (ownership_pct IS NULL OR (ownership_pct > 0 AND ownership_pct <= 100))
        AND (voting_pct IS NULL OR (voting_pct > 0 AND voting_pct <= 100))),
    CONSTRAINT or_kind_has_its_fact CHECK (
        (relationship_kind = 'OWNERSHIP' AND ownership_pct IS NOT NULL)
        OR (relationship_kind = 'CONTROL' AND control_type IS NOT NULL)),
    CONSTRAINT or_valid_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT or_version_positive CHECK (record_version > 0),
    CONSTRAINT or_evidence_present CHECK (btrim(evidence_ref) <> '' AND btrim(reason) <> ''),
    -- s4.4 SoD, enforced by the database as well as the service.
    CONSTRAINT or_no_self_approval CHECK (
        decided_by_principal_id IS NULL
        OR decided_by_principal_id <> proposed_by_principal_id),
    -- A decision names who and when; a PROPOSED row has neither.
    CONSTRAINT or_decision_consistent CHECK (
        (status = 'PROPOSED' AND decided_by_principal_id IS NULL AND decided_at IS NULL)
        OR (status <> 'PROPOSED' AND decided_by_principal_id IS NOT NULL AND decided_at IS NOT NULL))
);

-- The same relationship cannot be proposed twice for the same start while a
-- proposal or an active fact exists (s4.4 identity: parent+child+type+interval).
CREATE UNIQUE INDEX or_one_live_per_identity
    ON ownership_relationships
       (tenant_id, parent_legal_entity_id, child_legal_entity_id, relationship_kind, valid_from)
    WHERE status IN ('PROPOSED', 'ACTIVE');

CREATE INDEX idx_or_child  ON ownership_relationships (tenant_id, child_legal_entity_id, valid_from);
CREATE INDEX idx_or_parent ON ownership_relationships (tenant_id, parent_legal_entity_id, valid_from);

ALTER TABLE ownership_relationships ENABLE ROW LEVEL SECURITY;
ALTER TABLE ownership_relationships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON ownership_relationships
    FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
