-- Migration: 000002_add_location.up.sql
--
-- INV-02 (Inventory Location): "owns InventoryLocation. Must never own:
-- Legal ownership changes or inventory quantity/value." Fuller ownership
-- (verbatim): "InventoryLocation; warehouse/site/bin/quarantine/transit
-- location; parent hierarchy; custody entity; operational state;
-- location type; counting/negative-stock attributes." Purpose (verbatim):
-- "Maintain authoritative physical/custody inventory-location hierarchy
-- and operational eligibility without inferring legal ownership from
-- physical location."
--
-- State model (verbatim): "Draft→Active→Suspended/Quarantine→Retired;
-- hierarchy changes versioned; physical state separate from item
-- quantity." The same doc-vs-command mismatch pattern recurring
-- throughout this build hits INV-02 too: SuspendLocation has no reverse
-- command (no "ReactivateLocation" anywhere in the spec's own command
-- list) — SUSPENDED is a dead end in this v1, stated honestly.
-- SetQuarantineState, by contrast, IS its own real toggle — one command
-- that both enters AND releases quarantine (ACTIVE <-> QUARANTINE),
-- matching its own "Set...State" naming.
--
-- Minimum negative-path acceptance (verbatim, all four):
--   1. "Physical location change silently changes legal owner" —
--      legal_entity_id is set once at CreateInventoryLocation and is
--      never an editable field of AmendLocationMetadata (compile-time,
--      not just a runtime check) nor of ReparentLocationControlled
--      (which only ever touches parent_location_id). ReparentLocationControlled
--      additionally refuses, inside the same transaction, to attach a
--      location under a parent whose own legal_entity_id differs — the
--      same app-level cross-entity check AST-01's own MergeAssets uses
--      (a CHECK constraint cannot reference another row, so this is the
--      correct home for it, matching that precedent).
--   2. "Circular warehouse/bin hierarchy created" — ReparentLocationControlled
--      walks the candidate parent's own current ancestor chain, inside
--      the same transaction, and refuses if the location being reparented
--      appears anywhere in it (including as the candidate parent itself).
--   3. "Movement enters retired location" — INV-02 does not itself move
--      stock (that is INV-03's own future authority); its contribution is
--      making RETIRED a real terminal status (no command transitions out
--      of it) and ListEligibleLocations returning ACTIVE locations only,
--      for INV-03 to filter against once built. Stated honestly: the
--      actual movement-time block is deferred to INV-03, the same posture
--      INV-01 already took for its own negative paths #3/#4.
--   4. "Quarantine released without authority" — the spec's own SoD,
--      "quarantine release can require independent approval," is enforced
--      the same maker/checker way as every other approval step this
--      session: the principal who releases a quarantine must differ from
--      the principal who set it, refused universally (no finer materiality
--      concept exists in this platform, the same bootstrap-gap posture
--      AST-02's own ApproveDepreciationRun took).
--
-- inventory_location_hierarchy is versioned exactly like INV-01's own
-- tracking/valuation policies (migration 000001) and AST-02's own
-- DepreciationSchedule: a location's parent is never mutated in place,
-- only superseded by a new effective-dated version — the real,
-- structural answer to "hierarchy changes versioned." GetLocationAsOf
-- reads directly from this table.
CREATE TABLE inventory_locations (
    location_id                UUID PRIMARY KEY,
    tenant_id                    VARCHAR(255) NOT NULL,
    legal_entity_id                 VARCHAR(255) NOT NULL, -- immutable after creation — see negative path #1
    location_code                     VARCHAR(100) NOT NULL,
    location_type                       VARCHAR(30) NOT NULL, -- WAREHOUSE|SITE|BIN|QUARANTINE_AREA|TRANSIT
    description                           TEXT,
    custodian_entity                        VARCHAR(255), -- "custody entity" — deliberately separate from legal_entity_id per the spec's own purpose statement
    status                                     VARCHAR(20) NOT NULL, -- DRAFT|ACTIVE|SUSPENDED|QUARANTINE|RETIRED
    created_at                                   TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                        VARCHAR(255) NOT NULL,
    activated_at                                     TIMESTAMP WITH TIME ZONE,
    activated_by_principal_id                          VARCHAR(255),
    suspended_at                                         TIMESTAMP WITH TIME ZONE,
    suspended_by_principal_id                              VARCHAR(255),
    suspension_reason                                        TEXT,
    quarantined_at                                             TIMESTAMP WITH TIME ZONE,
    quarantined_by_principal_id                                  VARCHAR(255),
    quarantine_reason                                              TEXT,
    released_at                                                      TIMESTAMP WITH TIME ZONE,
    released_by_principal_id                                           VARCHAR(255),
    retired_at                                                           TIMESTAMP WITH TIME ZONE,
    retired_by_principal_id                                                VARCHAR(255),
    retirement_reason                                                        TEXT,

    CONSTRAINT chk_inventory_location_type CHECK (location_type IN ('WAREHOUSE', 'SITE', 'BIN', 'QUARANTINE_AREA', 'TRANSIT')),
    CONSTRAINT chk_inventory_location_status CHECK (status IN ('DRAFT', 'ACTIVE', 'SUSPENDED', 'QUARANTINE', 'RETIRED')),
    UNIQUE (tenant_id, legal_entity_id, location_code)
);

CREATE TABLE inventory_location_hierarchy (
    hierarchy_version_id      UUID PRIMARY KEY,
    version                      INT NOT NULL,
    tenant_id                      VARCHAR(255) NOT NULL,
    location_id                      UUID NOT NULL REFERENCES inventory_locations(location_id),
    parent_location_id                 UUID REFERENCES inventory_locations(location_id), -- NULL = root
    effective_from                       TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to                           TIMESTAMP WITH TIME ZONE, -- NULL = current
    created_at                               TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                    VARCHAR(255) NOT NULL
);

CREATE UNIQUE INDEX idx_location_hierarchy_current
    ON inventory_location_hierarchy (tenant_id, location_id) WHERE effective_to IS NULL;

ALTER TABLE inventory_locations ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_locations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_locations
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_location_hierarchy ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_location_hierarchy FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_location_hierarchy
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_inventory_locations_entity ON inventory_locations (tenant_id, legal_entity_id);
CREATE INDEX idx_location_hierarchy_location ON inventory_location_hierarchy (tenant_id, location_id);
CREATE INDEX idx_location_hierarchy_parent ON inventory_location_hierarchy (tenant_id, parent_location_id);
