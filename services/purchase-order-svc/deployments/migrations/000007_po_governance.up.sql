-- Migration 000007: AP-03 governance (ZS-SVC-D-001 §6).
--
-- 000004-000006 added lines, the expanded status set, progress tracking and the
-- outbox table. This migration adds what they lack and, because the runtime
-- role is a Postgres superuser (it bypasses GRANT/REVOKE and RLS), puts the
-- invariants in triggers — the only enforcement that applies to it:
--
--   * Draft -> PendingApproval -> Approved -> Issued is the only path to
--     ISSUED; a PO cannot be issued without approval evidence.
--   * An approved/issued PO is never edited in place: changing a commercial
--     field requires going back to DRAFT through a NEW revision whose prior
--     revision has been snapshotted immutably.
--   * Lines are only editable while the PO is DRAFT (progress counters aside).
--   * Revisions, progress and the event history are append-only.
--
-- Existing ISSUED/CLOSED rows stay valid: they are back-filled with
-- approval_basis = 'LEGACY' and keep their identity, status and totals.

-- 1. Header columns ----------------------------------------------------------

ALTER TABLE purchase_orders
    ADD COLUMN IF NOT EXISTS supplier_ref             TEXT,
    ADD COLUMN IF NOT EXISTS prepared_by_principal_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS approval_basis           VARCHAR(30),
    ADD COLUMN IF NOT EXISTS approval_ref             TEXT,
    ADD COLUMN IF NOT EXISTS held_from_status         VARCHAR(20),
    ADD COLUMN IF NOT EXISTS supplier_exception_ref   TEXT,
    ADD COLUMN IF NOT EXISTS supplier_exception_by    VARCHAR(255),
    ADD COLUMN IF NOT EXISTS delivery_terms           TEXT,
    ADD COLUMN IF NOT EXISTS payment_terms            TEXT;

-- A draft has not been issued yet.
ALTER TABLE purchase_orders ALTER COLUMN issued_by_principal_id DROP NOT NULL;
ALTER TABLE purchase_orders ALTER COLUMN issued_at DROP NOT NULL;
ALTER TABLE purchase_orders ALTER COLUMN issued_at DROP DEFAULT;

-- Existing orders were prepared and issued by the same principal, and carry no
-- separate approval record: mark them LEGACY so they remain valid ISSUED/CLOSED
-- records without pretending an approval happened.
UPDATE purchase_orders
   SET prepared_by_principal_id = issued_by_principal_id,
       approval_basis = 'LEGACY'
 WHERE prepared_by_principal_id IS NULL;

ALTER TABLE purchase_orders
    ADD CONSTRAINT purchase_orders_approval_basis_check
    CHECK (approval_basis IS NULL OR approval_basis IN ('LEGACY', 'WORKFLOW', 'PURCHASE_REQUEST', 'PROCUREMENT_CASE'));

-- hold/release remembers where to return to
ALTER TABLE purchase_orders
    ADD CONSTRAINT purchase_orders_held_from_check
    CHECK (held_from_status IS NULL OR held_from_status IN ('APPROVED', 'ISSUED'));

-- 2. Immutable revisions -------------------------------------------------------
-- One row per superseded revision of an approved/issued PO: the full header and
-- line set as it stood. Never updated or deleted.

CREATE TABLE purchase_order_revisions (
    revision_id              UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL,
    purchase_order_id        UUID NOT NULL REFERENCES purchase_orders (purchase_order_id),
    revision                 INTEGER NOT NULL,
    status_at_snapshot       VARCHAR(20) NOT NULL,
    snapshot                 JSONB NOT NULL,
    approved_by_principal_id VARCHAR(255),
    approved_at              TIMESTAMPTZ,
    reason                   TEXT NOT NULL DEFAULT '',
    created_by_principal_id  VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX uq_po_revisions_order_revision ON purchase_order_revisions (purchase_order_id, revision);
CREATE INDEX idx_po_revisions_tenant ON purchase_order_revisions (tenant_id);

-- 3. Append-only event history ------------------------------------------------

CREATE TABLE purchase_order_events (
    event_id           UUID PRIMARY KEY,
    tenant_id          UUID NOT NULL,
    purchase_order_id  UUID NOT NULL REFERENCES purchase_orders (purchase_order_id),
    event_type         VARCHAR(64) NOT NULL,
    from_status        VARCHAR(20),
    to_status          VARCHAR(20),
    revision           INTEGER NOT NULL,
    version            INTEGER NOT NULL,
    detail             TEXT NOT NULL DEFAULT '',
    actor_principal_id VARCHAR(255) NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_po_events_order ON purchase_order_events (purchase_order_id, created_at);
CREATE INDEX idx_po_events_tenant ON purchase_order_events (tenant_id);

-- 4. Idempotency keys ---------------------------------------------------------
-- One row per (tenant, command scope, Idempotency-Key); the request hash binds a
-- key to one exact request.

CREATE TABLE idempotency_keys (
    tenant_key    TEXT NOT NULL,
    scope         TEXT NOT NULL,
    idem_key      TEXT NOT NULL,
    request_hash  TEXT NOT NULL,
    status_code   INT NULL,
    response_body BYTEA NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at  TIMESTAMPTZ NULL,
    PRIMARY KEY (tenant_key, scope, idem_key)
);
CREATE INDEX idx_po_idempotency_created ON idempotency_keys (created_at);

-- 5. RLS on the new tables (same strict policy as 000004-000006) ----------------

ALTER TABLE purchase_order_revisions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purchase_order_revisions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
ALTER TABLE purchase_order_revisions FORCE ROW LEVEL SECURITY;

ALTER TABLE purchase_order_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purchase_order_events
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
ALTER TABLE purchase_order_events FORCE ROW LEVEL SECURITY;

-- 6. Triggers -------------------------------------------------------------------

CREATE OR REPLACE FUNCTION po_reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'rows in % are append-only and can never be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_po_revisions_append_only
    BEFORE UPDATE OR DELETE ON purchase_order_revisions
    FOR EACH ROW EXECUTE FUNCTION po_reject_evidence_mutation();
CREATE TRIGGER trg_po_events_append_only
    BEFORE UPDATE OR DELETE ON purchase_order_events
    FOR EACH ROW EXECUTE FUNCTION po_reject_evidence_mutation();
CREATE TRIGGER trg_po_progress_append_only
    BEFORE UPDATE OR DELETE ON purchase_order_progress
    FOR EACH ROW EXECUTE FUNCTION po_reject_evidence_mutation();

-- 6a. purchase_orders: state machine, terminal immutability, no in-place edits.
CREATE OR REPLACE FUNCTION po_guard_order() RETURNS TRIGGER AS $$
DECLARE
    allowed BOOLEAN := FALSE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'purchase_orders rows are never deleted';
    END IF;

    IF TG_OP = 'INSERT' THEN
        -- A PO is born DRAFT (the governed flow) or ISSUED with a recorded
        -- approval basis (the legacy direct-issue path, which verifies its
        -- approval at the source). Never APPROVED/PENDING/etc.
        IF NEW.po_status = 'ISSUED' AND NEW.approval_basis IS NULL THEN
            RAISE EXCEPTION 'a purchase order cannot be created ISSUED without an approval basis';
        END IF;
        IF NEW.po_status NOT IN ('DRAFT', 'ISSUED') THEN
            RAISE EXCEPTION 'a purchase order cannot be created in status %', NEW.po_status;
        END IF;
        RETURN NEW;
    END IF;

    -- UPDATE
    IF OLD.po_status IN ('CLOSED', 'CANCELLED') THEN
        RAISE EXCEPTION 'purchase order % is %, which is terminal', OLD.purchase_order_id, OLD.po_status;
    END IF;

    IF NEW.purchase_order_id IS DISTINCT FROM OLD.purchase_order_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.po_number IS DISTINCT FROM OLD.po_number
        OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'purchase order % identity fields can never change', OLD.purchase_order_id;
    END IF;

    IF NEW.po_status IS DISTINCT FROM OLD.po_status THEN
        allowed := CASE OLD.po_status
            WHEN 'DRAFT'            THEN NEW.po_status IN ('PENDING_APPROVAL', 'CANCELLED')
            WHEN 'PENDING_APPROVAL' THEN NEW.po_status IN ('APPROVED', 'DRAFT', 'CANCELLED')
            WHEN 'APPROVED'         THEN NEW.po_status IN ('ISSUED', 'DRAFT', 'ON_HOLD', 'CANCELLED')
            WHEN 'ISSUED'           THEN NEW.po_status IN ('ON_HOLD', 'DRAFT', 'CLOSED', 'CANCELLED')
            WHEN 'ON_HOLD'          THEN NEW.po_status IN ('APPROVED', 'ISSUED', 'CANCELLED')
            ELSE FALSE
        END;
        IF NOT allowed THEN
            RAISE EXCEPTION 'purchase order % cannot move from % to %', OLD.purchase_order_id, OLD.po_status, NEW.po_status;
        END IF;

        -- ISSUED is reachable only after approval (or back from a hold).
        IF NEW.po_status = 'ISSUED' AND OLD.po_status = 'APPROVED'
           AND (NEW.approved_by_principal_id IS NULL OR NEW.approved_at IS NULL OR NEW.approval_basis IS NULL) THEN
            RAISE EXCEPTION 'purchase order % cannot be issued without approval evidence', OLD.purchase_order_id;
        END IF;
        IF NEW.po_status = 'APPROVED' AND OLD.po_status = 'PENDING_APPROVAL'
           AND (NEW.approved_by_principal_id IS NULL OR NEW.approved_at IS NULL) THEN
            RAISE EXCEPTION 'purchase order % cannot be approved without an approver', OLD.purchase_order_id;
        END IF;
        -- Maker-checker: the preparer can never be the approver. Only an approval
        -- verified at an external source (a requisition or a procurement case, whose
        -- own approver is recorded there) is exempt; the in-service WORKFLOW
        -- approval is exactly what this rule exists for.
        IF NEW.po_status = 'APPROVED' AND OLD.po_status = 'PENDING_APPROVAL'
           AND NEW.approved_by_principal_id IS NOT DISTINCT FROM OLD.prepared_by_principal_id
           AND OLD.prepared_by_principal_id IS NOT NULL
           AND COALESCE(NEW.approval_basis, '') NOT IN ('PURCHASE_REQUEST', 'PROCUREMENT_CASE') THEN
            RAISE EXCEPTION 'the preparer of purchase order % cannot approve it', OLD.purchase_order_id;
        END IF;

        -- Leaving an approved/issued state for DRAFT is an amendment: it must
        -- come with the next revision number AND the superseded revision
        -- snapshotted immutably. That is what makes "overwritten in place"
        -- impossible at the database.
        IF OLD.po_status IN ('APPROVED', 'ISSUED') AND NEW.po_status = 'DRAFT' THEN
            IF NEW.revision <> OLD.revision + 1 THEN
                RAISE EXCEPTION 'amending approved purchase order % requires revision % (got %)', OLD.purchase_order_id, OLD.revision + 1, NEW.revision;
            END IF;
            IF NOT EXISTS (SELECT 1 FROM purchase_order_revisions r
                            WHERE r.purchase_order_id = OLD.purchase_order_id AND r.revision = OLD.revision) THEN
                RAISE EXCEPTION 'amending approved purchase order % requires revision % to be snapshotted first', OLD.purchase_order_id, OLD.revision;
            END IF;
        END IF;
    END IF;

    -- Commercial fields of an approved/issued/held order cannot be edited in
    -- place; the only way out is the DRAFT branch above.
    IF OLD.po_status IN ('APPROVED', 'ISSUED', 'ON_HOLD') AND NEW.po_status IS NOT DISTINCT FROM OLD.po_status THEN
        IF NEW.total_amount IS DISTINCT FROM OLD.total_amount
            OR NEW.currency_code IS DISTINCT FROM OLD.currency_code
            OR NEW.supplier_ref IS DISTINCT FROM OLD.supplier_ref
            OR NEW.vendor_profile_id IS DISTINCT FROM OLD.vendor_profile_id
            OR NEW.payment_terms IS DISTINCT FROM OLD.payment_terms THEN
            RAISE EXCEPTION 'purchase order % is % and its commercial terms cannot be edited in place; amend it as a new revision', OLD.purchase_order_id, OLD.po_status;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_po_guard_order
    BEFORE INSERT OR UPDATE OR DELETE ON purchase_orders
    FOR EACH ROW EXECUTE FUNCTION po_guard_order();

-- 6b. purchase_order_lines: editable only while the PO is DRAFT, apart from the
--     progress counters and non-commercial descriptive fields.
CREATE OR REPLACE FUNCTION po_guard_line() RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        SELECT po_status INTO parent_status FROM purchase_orders WHERE purchase_order_id = OLD.purchase_order_id;
        IF parent_status IS DISTINCT FROM 'DRAFT' THEN
            RAISE EXCEPTION 'lines of purchase order % can only be removed while it is DRAFT (it is %)', OLD.purchase_order_id, parent_status;
        END IF;
        IF OLD.received_quantity <> 0 OR OLD.invoiced_quantity <> 0 THEN
            RAISE EXCEPTION 'line % has receipt/invoice progress and cannot be removed', OLD.line_id;
        END IF;
        RETURN OLD;
    END IF;

    SELECT po_status INTO parent_status FROM purchase_orders WHERE purchase_order_id = NEW.purchase_order_id;

    IF TG_OP = 'INSERT' THEN
        IF parent_status IS DISTINCT FROM 'DRAFT' THEN
            RAISE EXCEPTION 'lines can only be added to purchase order % while it is DRAFT (it is %)', NEW.purchase_order_id, parent_status;
        END IF;
        RETURN NEW;
    END IF;

    -- UPDATE
    IF NEW.line_id IS DISTINCT FROM OLD.line_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.purchase_order_id IS DISTINCT FROM OLD.purchase_order_id
        OR NEW.line_number IS DISTINCT FROM OLD.line_number THEN
        RAISE EXCEPTION 'line identity can never change';
    END IF;

    IF NEW.item_ref IS DISTINCT FROM OLD.item_ref
        OR NEW.quantity IS DISTINCT FROM OLD.quantity
        OR NEW.unit_price IS DISTINCT FROM OLD.unit_price
        OR NEW.uom IS DISTINCT FROM OLD.uom
        OR NEW.line_amount IS DISTINCT FROM OLD.line_amount THEN
        IF parent_status IS DISTINCT FROM 'DRAFT' THEN
            RAISE EXCEPTION 'commercial fields of line % can only change while purchase order is DRAFT (it is %)', OLD.line_id, parent_status;
        END IF;
    END IF;

    -- Progress counters never go negative.
    IF NEW.received_quantity < 0 OR NEW.invoiced_quantity < 0 THEN
        RAISE EXCEPTION 'line % progress cannot be negative', OLD.line_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_po_guard_line
    BEFORE INSERT OR UPDATE OR DELETE ON purchase_order_lines
    FOR EACH ROW EXECUTE FUNCTION po_guard_line();

-- Positive quantities and prices, and a consistent line amount.
ALTER TABLE purchase_order_lines
    ADD CONSTRAINT purchase_order_lines_quantity_positive CHECK (quantity > 0),
    ADD CONSTRAINT purchase_order_lines_unit_price_nonneg CHECK (unit_price >= 0),
    ADD CONSTRAINT purchase_order_lines_amount_nonneg CHECK (line_amount >= 0);

-- 7. Lookups ----------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS idx_purchase_orders_supplier ON purchase_orders (tenant_id, supplier_ref) WHERE supplier_ref IS NOT NULL;

-- 8. outbox_events is an infrastructure queue, not tenant data ------------------
-- 000006 put forced row-level security on it. The relay reads it ACROSS tenants
-- with no tenant set, so under an ordinary NOBYPASSRLS role (which
-- docker-compose gives this service: app_purchase_order) the policy hides every
-- row and no event is ever published — silently. Every other service's outbox
-- table omits RLS for exactly this reason; the rows carry tenant_id and are only
-- ever written by this service's own transactions and read by its relay.
DROP POLICY IF EXISTS tenant_isolation_policy ON outbox_events;
ALTER TABLE outbox_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;
