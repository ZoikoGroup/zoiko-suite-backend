-- Migration: 000003_add_movement.up.sql
--
-- INV-03 (Inventory Movement): "owns InventoryMovement. Must never own:
-- Monetary valuation policy or direct GL writes." Fuller ownership
-- (verbatim): "InventoryMovement; movement line; source/destination;
-- quantity/UOM; item/lot/serial; business/effective/posting dates;
-- source reference; movement type; reversal/supersession link." Purpose
-- (verbatim): "Record immutable quantity movements across external
-- boundaries and locations so on-hand quantity is derived from events
-- rather than editable balance fields."
--
-- This last clause is the whole architecture: there is NO on-hand
-- balance table anywhere in this schema. GetOnHand/GetOnHandAsOf (see
-- the store) always compute live from SUM(committed movement quantities)
-- — the real, structural answer to "derived from events rather than
-- editable balance fields." A balance table that could itself be edited
-- would be exactly the bug this purpose statement rules out.
--
-- State model (verbatim): "Draft→Validated→Committed→Valued→Reconciled;
-- committed movement immutable; reversed/superseded separately." No
-- command in the spec's own list reaches Valued or Reconciled — both
-- depend on INV-04 (Valuation), not yet built — left unreachable in this
-- v1, the same posture as AST-02's own Reconciled/Certified gap.
-- "Committed movement immutable" is enforced literally and completely:
-- trg_reject_movement_mutation blocks EVERY update to a COMMITTED row,
-- not just economic fields (AST-02/03's own append-only guards protect
-- only the economic subset; here the spec's own word is "immutable," full
-- stop, so the guard is a full block). ReverseMovement/SupersedeMovement
-- never touch the original row — each creates a brand-new movement with
-- source/destination swapped (the mechanical inverse of any movement
-- type), linked back via reverses_movement_id/supersedes_movement_id.
--
-- Explicit commands (verbatim): "CreateInventoryMovement; ValidateMovement;
-- CommitMovement; ReverseMovement; SupersedeMovement; TransferInventory;
-- ReceiveInventory; IssueInventory; AdjustInventoryFromApprovedCount."
-- TransferInventory/ReceiveInventory/IssueInventory/
-- AdjustInventoryFromApprovedCount are built as thin, type-specific entry
-- points into the one real create path (CreateInventoryMovement), each
-- pre-setting movement_type and enforcing that type's own required field
-- shape — the same pattern AST-03's RecordDisposal/RecordImpairment/etc.
-- already established. That field-shape difference is also the real
-- enforcement of the spec's own SoD, "source domain cannot create
-- arbitrary inventory adjustment disguised as receipt/issue":
-- AdjustInventoryFromApprovedCount is its own distinct authz action
-- (inventory.movement.adjust, not inventory.movement.create) and its own
-- required count_reference field — there is no way to reach an ADJUSTMENT
-- row through ReceiveInventory/IssueInventory's own code paths.
--
-- Minimum negative-path acceptance (verbatim, all four):
--   1. "Duplicate receipt creates duplicate quantity" — the spec's own
--      failure semantics say "Duplicate idempotency key returns original
--      result," so this is NOT a rejection: CreateInventoryMovement is a
--      real idempotent create backed by
--      UNIQUE(tenant_id, source_idempotency_key) — a retried call with
--      the same key resolves to and returns the ALREADY-CREATED movement
--      rather than erroring or creating a second one.
--   2. "Serial-numbered unit appears in two locations" — inventory_serial_residency
--      tracks, per (tenant, item, serial), the ONE location that serial
--      currently resides at (or no row at all if it has left the
--      system). CommitMovement — for a serial-tracked item, verified
--      against INV-01's own current TrackingPolicy — refuses to receive a
--      serial that already has a residency row, and refuses to
--      ship/transfer a serial that has no residency row at the claimed
--      source location, all inside the same transaction as the commit.
--   3. "Negative stock allowed despite policy prohibition" — CommitMovement
--      computes live on-hand for (item, location) inside its own
--      transaction before allowing any quantity-decreasing movement
--      (ISSUE, TRANSFER's source side, or a decreasing ADJUSTMENT) and
--      refuses if the result would go below zero. No per-item/location
--      override policy exists yet in this v1 — negative stock is refused
--      universally, stated honestly as the same "no finer policy concept
--      yet" bootstrap gap this session has hit repeatedly.
--   4. "Hard-closed movement backdated without correction path" —
--      CommitMovement calls financial-close-svc's real period-status
--      endpoint (internal/clients.Clients.CheckPeriodOpen, mirroring
--      asset-management-svc's own AST-03 client exactly) before
--      committing; a LOCKED/CLOSED period refuses, an unreachable
--      financial-close-svc fails CLOSED.
--
-- ValidateMovement is also where INV-01's own promised-but-deferred
-- negative path lands for real: "Lot-tracked item moved without lot
-- identity" (INV-01's migration 000001, negative path #3) is enforced
-- here — a movement against an item whose current TrackingPolicy
-- requires lot/serial tracking is refused if lot_number/serial_number is
-- missing. Likewise INV-02's own deferred "Movement enters retired
-- location" (migration 000002, negative path #3) lands here too:
-- ValidateMovement refuses against any source/destination location that
-- isn't ACTIVE.
CREATE TABLE inventory_movements (
    movement_id                  UUID PRIMARY KEY,
    tenant_id                      VARCHAR(255) NOT NULL,
    legal_entity_id                  VARCHAR(255) NOT NULL,
    movement_type                      VARCHAR(20) NOT NULL, -- RECEIPT|ISSUE|TRANSFER|ADJUSTMENT|REVERSAL|SUPERSESSION
    status                                VARCHAR(20) NOT NULL, -- DRAFT|VALIDATED|COMMITTED (VALUED/RECONCILED unreachable in this v1)
    item_id                                UUID NOT NULL REFERENCES inventory_items(item_id),
    source_location_id                       UUID REFERENCES inventory_locations(location_id), -- NULL for RECEIPT (external source)
    destination_location_id                    UUID REFERENCES inventory_locations(location_id), -- NULL for ISSUE (external destination)
    quantity                                     NUMERIC(18,4) NOT NULL,
    uom                                             VARCHAR(20) NOT NULL,
    lot_number                                       VARCHAR(100),
    serial_number                                      VARCHAR(100),
    source_reference                                     VARCHAR(255) NOT NULL, -- required business/source input
    source_idempotency_key                                 VARCHAR(255) NOT NULL, -- negative path #1
    business_date                                            DATE NOT NULL,
    fiscal_period                                              VARCHAR(20) NOT NULL,
    reverses_movement_id                                         UUID REFERENCES inventory_movements(movement_id),
    supersedes_movement_id                                         UUID REFERENCES inventory_movements(movement_id),
    reason                                                           TEXT, -- ADJUSTMENT/REVERSAL/SUPERSESSION evidence

    created_at                  TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id       VARCHAR(255) NOT NULL,
    validated_at                    TIMESTAMP WITH TIME ZONE,
    committed_at                      TIMESTAMP WITH TIME ZONE,
    committed_by_principal_id           VARCHAR(255),

    CONSTRAINT chk_inventory_movement_type CHECK (movement_type IN ('RECEIPT', 'ISSUE', 'TRANSFER', 'ADJUSTMENT', 'REVERSAL', 'SUPERSESSION')),
    CONSTRAINT chk_inventory_movement_status CHECK (status IN ('DRAFT', 'VALIDATED', 'COMMITTED')),
    CONSTRAINT chk_inventory_movement_quantity_positive CHECK (quantity > 0),
    UNIQUE (tenant_id, source_idempotency_key)
);

-- Negative path (from the state model itself), "committed movement
-- immutable" — a full block, not merely an economic-field subset (unlike
-- AST-02/03's own append-only guards): once COMMITTED, this row never
-- changes again, for any reason, through any code path.
CREATE OR REPLACE FUNCTION reject_movement_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status = 'COMMITTED' THEN
        RAISE EXCEPTION 'inventory_movements: a COMMITTED movement is immutable — % is not permitted', TG_OP;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_movement_mutation
    BEFORE UPDATE ON inventory_movements
    FOR EACH ROW EXECUTE FUNCTION reject_movement_mutation();

-- inventory_serial_residency is real, current-state evidence — negative
-- path #2's own enforcement mechanism. One row per serial that currently
-- resides somewhere in the tracked estate; deleted when the serial
-- leaves via ISSUE (ships out of the tracked boundary).
CREATE TABLE inventory_serial_residency (
    tenant_id       VARCHAR(255) NOT NULL,
    item_id           UUID NOT NULL REFERENCES inventory_items(item_id),
    serial_number       VARCHAR(100) NOT NULL,
    location_id           UUID NOT NULL REFERENCES inventory_locations(location_id),
    updated_at              TIMESTAMP WITH TIME ZONE NOT NULL,

    PRIMARY KEY (tenant_id, item_id, serial_number)
);

ALTER TABLE inventory_movements ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_movements FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_movements
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE inventory_serial_residency ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_serial_residency FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON inventory_serial_residency
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_inventory_movements_entity ON inventory_movements (tenant_id, legal_entity_id);
CREATE INDEX idx_inventory_movements_item ON inventory_movements (tenant_id, item_id);
CREATE INDEX idx_inventory_movements_source_loc ON inventory_movements (tenant_id, source_location_id) WHERE source_location_id IS NOT NULL;
CREATE INDEX idx_inventory_movements_dest_loc ON inventory_movements (tenant_id, destination_location_id) WHERE destination_location_id IS NOT NULL;
CREATE INDEX idx_inventory_movements_reverses ON inventory_movements (tenant_id, reverses_movement_id) WHERE reverses_movement_id IS NOT NULL;
