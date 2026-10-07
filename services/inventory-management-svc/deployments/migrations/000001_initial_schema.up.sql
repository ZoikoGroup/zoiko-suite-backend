-- Migration: 000001_initial_schema.up.sql
--
-- INV-01 (Item / Product Master): "owns InventoryItem. Must never own:
-- Commercial offer/pricing authority or historic movement
-- reinterpretation." Fuller ownership (verbatim): "InventoryItem;
-- item/SKU identity; stocking/base UOM; item type; lot/serial/expiry
-- policy; inventory classification; entity inventory profile;
-- valuation-policy reference; lifecycle." Purpose (verbatim): "Maintain
-- canonical stockkeeping and inventory-accounting attributes for
-- physical/stock items while remaining distinct from the commercial
-- Product & Service Catalog."
--
-- State model (verbatim): "Draft→Active→Suspended→Retired; valuation/
-- tracking policy versions are effective-dated and do not rewrite
-- historic movements."
--
-- Explicit commands (verbatim): "CreateInventoryItem; ActivateInventoryItem;
-- AmendInventoryProfile; SetTrackingPolicy; SetValuationPolicyFutureEffective;
-- RetireInventoryItem; LinkCommercialCatalogItem." The same doc-vs-command
-- mismatch pattern already found throughout this platform (ACC-08/09/10/17,
-- AST-01/02/03) recurs here: no command named SuspendInventoryItem or
-- ReactivateInventoryItem exists, despite "Suspended" being named in the
-- state model — left unreachable in this v1, stated honestly, the same
-- posture as AST-02's own Reconciled/Certified gap. This v1's only real
-- transitions are DRAFT -> ACTIVE (ActivateInventoryItem) and
-- ACTIVE -> RETIRED (RetireInventoryItem).
--
-- Minimum negative-path acceptance (verbatim, all four):
--   1. "Commercial catalog changes inventory valuation method" —
--      LinkCommercialCatalogItem's own request carries only a
--      catalog_item_id reference; there is no code path from that command
--      to inventory_valuation_policies at all — structurally impossible,
--      not merely unauthorized. LinkCommercialCatalogItem and
--      SetValuationPolicyFutureEffective are also distinct authz actions
--      (inventory.item.manage vs inventory.policy.assign) — the spec's own
--      SoD: "commercial pricing/catalog editors cannot alter inventory
--      accounting policy."
--   2. "Historic movement reinterpreted using new UOM mapping" —
--      base_uom is set once at CreateInventoryItem and is never an
--      editable field of AmendInventoryProfile (see handler) — no command
--      on this platform can change an item's base UOM after creation, so a
--      past movement's UOM can never be silently reinterpreted.
--   3. "Lot-tracked item moved without lot identity" — INV-01 does not
--      itself move stock (that is INV-03's own future authority); its
--      contribution is recording a real, queryable RequiresLotTracking/
--      RequiresSerialTracking/RequiresExpiryTracking policy INV-03 will
--      be built to check before accepting a movement. Stated honestly:
--      the actual movement-time block is deferred to INV-03.
--   4. "Retired item accepts new receipt without override" — same
--      posture: RETIRED is a real terminal status (no command transitions
--      out of it — see the handler's own transition guards), so INV-03,
--      once built, can refuse a receipt against a non-ACTIVE item. INV-01's
--      own contribution is making that status durable and queryable.
--
-- inventory_tracking_policies and inventory_valuation_policies are
-- versioned exactly like AST-02's own DepreciationSchedule (migration
-- 000002 in asset-management-svc) and ACC-09's AllocationRule: a stable
-- logical policy_id carrying effective-dated versions, never mutated in
-- place. SetValuationPolicyFutureEffective's own name is the spec's own
-- structural answer to its SoD requirement, "Retroactive valuation-policy
-- change requires controlled migration/revaluation approval" — this v1
-- enforces "future" the simple, real way: effective_from must be later
-- than the moment the command runs (application-level check), and the
-- prior version is never end-dated before its own effective_to naturally
-- arrives — there is no in-place edit path to a policy's own effective
-- window, only a new future-dated version.
CREATE TABLE inventory_items (
    item_id                  UUID PRIMARY KEY,
    tenant_id                  VARCHAR(255) NOT NULL,
    legal_entity_id               VARCHAR(255) NOT NULL,
    sku                             VARCHAR(100) NOT NULL,
    description                       TEXT NOT NULL,
    base_uom                           VARCHAR(20) NOT NULL, -- REF UOM; caller-declared (REF UOM does not exist platform-wide — same bootstrap posture as book_id elsewhere), immutable after creation
    item_type                            VARCHAR(30), -- stock/non-stock classification
    physical_characteristics               TEXT,
    catalog_item_id                          VARCHAR(255), -- LinkCommercialCatalogItem's own link; BIZ-07 does not exist platform-wide, caller-declared reference only
    status                                     VARCHAR(20) NOT NULL, -- DRAFT|ACTIVE|SUSPENDED|RETIRED (SUSPENDED unreachable in this v1 — see doc comment above)
    created_at                                   TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                        VARCHAR(255) NOT NULL,
    activated_at                                     TIMESTAMP WITH TIME ZONE,
    activated_by_principal_id                          VARCHAR(255),
    retired_at                                           TIMESTAMP WITH TIME ZONE,
    retired_by_principal_id                                VARCHAR(255),
    retirement_reason                                        TEXT,

    CONSTRAINT chk_inventory_item_status CHECK (status IN ('DRAFT', 'ACTIVE', 'SUSPENDED', 'RETIRED')),
    UNIQUE (tenant_id, legal_entity_id, sku)
);

CREATE TABLE inventory_tracking_policies (
    policy_version_id      UUID PRIMARY KEY,
    policy_id                 UUID NOT NULL,
    version                     INT NOT NULL,
    tenant_id                     VARCHAR(255) NOT NULL,
    item_id                         UUID NOT NULL REFERENCES inventory_items(item_id),
    requires_lot_tracking             BOOLEAN NOT NULL DEFAULT FALSE,
    requires_serial_tracking           BOOLEAN NOT NULL DEFAULT FALSE,
    requires_expiry_tracking             BOOLEAN NOT NULL DEFAULT FALSE,
    effective_from                         TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to                             TIMESTAMP WITH TIME ZONE, -- NULL = current
    created_at                                 TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                      VARCHAR(255) NOT NULL
);

CREATE UNIQUE INDEX idx_tracking_policies_current
    ON inventory_tracking_policies (tenant_id, item_id) WHERE effective_to IS NULL;

CREATE TABLE inventory_valuation_policies (
    policy_version_id      UUID PRIMARY KEY,
    policy_id                 UUID NOT NULL,
    version                     INT NOT NULL,
    tenant_id                     VARCHAR(255) NOT NULL,
    item_id                         UUID NOT NULL REFERENCES inventory_items(item_id),
    valuation_method                 VARCHAR(30) NOT NULL,
    effective_from                     TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to                         TIMESTAMP WITH TIME ZONE, -- NULL = current
    created_at                             TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                  VARCHAR(255) NOT NULL,

    CONSTRAINT chk_inventory_valuation_method CHECK (valuation_method IN ('FIFO', 'WEIGHTED_AVERAGE', 'STANDARD_COST'))
);

CREATE UNIQUE INDEX idx_valuation_policies_current
    ON inventory_valuation_policies (tenant_id, item_id) WHERE effective_to IS NULL;

ALTER TABLE inventory_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_items FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_items
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_tracking_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_tracking_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_tracking_policies
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_valuation_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_valuation_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_valuation_policies
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_inventory_items_entity ON inventory_items (tenant_id, legal_entity_id);
CREATE INDEX idx_tracking_policies_item ON inventory_tracking_policies (tenant_id, item_id);
CREATE INDEX idx_valuation_policies_item ON inventory_valuation_policies (tenant_id, item_id);
