-- Migration: 000001_initial_schema.up.sql
--
-- AST-01 (Fixed Asset Register): "owns FixedAsset. Must never own: Posted
-- GL balances, depreciation calculations or free-form carrying-value
-- edits." Fuller ownership: "FixedAsset; AssetComponent; asset category/
-- class; tag/serial; ownership entity; custodian/location references;
-- acquisition/source references; in-service metadata; AssetBookProfile
-- linkage; lifecycle projection. It does not own posted GL balances."
--
-- State model (verbatim): "Candidate→Reviewed→Registered→Capitalized/
-- Active; Suspended/Disposed/HeldForSale are lifecycle dimensions driven
-- by accepted events; book states tracked separately." The doc names no
-- command that reaches "Reviewed" independently of Registered — only
-- ApproveAssetRegistration bridges Candidate to Registered — so this v1
-- collapses Reviewed into that one transition, the same "no separate
-- command exists for a named intermediate state" pattern already used in
-- the Accounting domain (e.g. ACC-17's Planned/Loaded collapse). Disposed
-- and HeldForSale are explicitly "driven by accepted events" — AST-03's
-- own authority, not reachable through any AST-01 command — so this
-- migration's own CHECK constraint on status intentionally excludes them
-- for now; AST-03's own migration will extend the allowed set when it is
-- built.
--
-- fixed_assets is a normal mutable stateful row walking the lifecycle
-- above — never append-only, since a real asset's custody/location and
-- non-financial metadata legitimately change over its life. What must
-- NEVER be directly editable is carrying/book value (the spec's own
-- negative path, "User edits carrying value directly") — this schema
-- structurally enforces that by never storing a mutable net-book-value
-- column at all; book values are AST-02's own derived authority.
--
-- asset_components and asset_book_assignments are child tables, not
-- separate top-level authorities — AddComponent/AssignAssetBookProfile
-- are the only write paths, both scoped to a parent asset_id.
CREATE TABLE fixed_assets (
    asset_id                UUID PRIMARY KEY,
    tenant_id                 VARCHAR(255) NOT NULL,
    legal_entity_id            VARCHAR(255) NOT NULL,
    asset_category              VARCHAR(100) NOT NULL,
    tag_serial                   VARCHAR(255),
    description                   TEXT NOT NULL,
    custodian_id                  VARCHAR(255),
    location_id                    VARCHAR(255),
    -- Required business/source inputs (§9.D-style — this doc's own
    -- "Required business/source inputs" field): the acquisition document
    -- this candidate is evidenced by. Capitalization is blocked without
    -- it — the spec's own negative path, "Asset capitalized without
    -- source evidence."
    acquisition_source_ref          VARCHAR(255),
    acquisition_date                 DATE,
    in_service_date                   DATE,
    status                             VARCHAR(20) NOT NULL, -- CANDIDATE|REGISTERED|ACTIVE|SUSPENDED|MERGED
    merged_into_asset_id                UUID REFERENCES fixed_assets(asset_id),
    -- split_from_asset_id records SplitAssetControlled's own lineage —
    -- this candidate was carved out of another asset's components, not
    -- independently acquired. Both this column and merged_into_asset_id
    -- back GetAssetSourceLineage without a separate lineage table.
    split_from_asset_id                 UUID REFERENCES fixed_assets(asset_id),
    created_at                          TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id             VARCHAR(255) NOT NULL,
    registered_at                       TIMESTAMP WITH TIME ZONE,
    registered_by_principal_id          VARCHAR(255),
    capitalized_at                      TIMESTAMP WITH TIME ZONE,
    capitalized_by_principal_id         VARCHAR(255),
    suspended_at                        TIMESTAMP WITH TIME ZONE,
    suspended_by_principal_id           VARCHAR(255),
    suspension_reason                   TEXT,

    CONSTRAINT chk_fixed_asset_status CHECK (status IN ('CANDIDATE', 'REGISTERED', 'ACTIVE', 'SUSPENDED', 'MERGED'))
);

CREATE TABLE asset_components (
    component_id             UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    asset_id                     UUID NOT NULL REFERENCES fixed_assets(asset_id),
    description                    TEXT NOT NULL,
    cost_source_ref                  VARCHAR(255),
    -- moved_to_asset_id is set when SplitAssetControlled carves this
    -- component off to a new asset record — the component row itself is
    -- never deleted, only marked moved, preserving its own history.
    moved_to_asset_id                 UUID REFERENCES fixed_assets(asset_id),
    created_at                        TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id           VARCHAR(255) NOT NULL
);

-- One book assignment per (asset, book) — AssignAssetBookProfile is
-- idempotent-by-replacement, never a second row for the same book.
-- UsefulLifeMonthsProposal/ResidualValueProposal are the spec's own named
-- "useful-life/residual-value proposals" required input — captured here
-- as AST-01's own evidence of what was proposed, never as an actual
-- depreciation parameter AST-01 itself calculates from (that authority is
-- AST-02's alone).
CREATE TABLE asset_book_assignments (
    assignment_id                  UUID PRIMARY KEY,
    tenant_id                        VARCHAR(255) NOT NULL,
    asset_id                           UUID NOT NULL REFERENCES fixed_assets(asset_id),
    book_id                              VARCHAR(255) NOT NULL, -- REF-06 Accounting Book does not exist platform-wide; caller-declared, same bootstrap-gap posture as general-ledger-svc's own book_id
    useful_life_months_proposal            INT,
    residual_value_proposal                 NUMERIC(18,2),
    status                                    VARCHAR(20) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE|SUSPENDED, tracked separately from the asset's own lifecycle
    assigned_at                               TIMESTAMP WITH TIME ZONE NOT NULL,
    assigned_by_principal_id                  VARCHAR(255) NOT NULL,

    UNIQUE (tenant_id, asset_id, book_id)
);

ALTER TABLE fixed_assets ENABLE ROW LEVEL SECURITY;
ALTER TABLE fixed_assets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fixed_assets
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE asset_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_components FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON asset_components
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE asset_book_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_book_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON asset_book_assignments
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_fixed_assets_entity ON fixed_assets (tenant_id, legal_entity_id);
CREATE INDEX idx_asset_components_asset ON asset_components (tenant_id, asset_id);
CREATE INDEX idx_asset_book_assignments_asset ON asset_book_assignments (tenant_id, asset_id);
