-- Migration: 000006_requisition_lifecycle.up.sql
--
-- AP-02 full requisition model (spec §5): Draft -> PendingApproval ->
-- Approved/Rejected -> Converted/Cancelled/Expired, lines, coding, budget
-- evidence, version column, append-only history, and DB-level guards so an
-- approved requisition cannot change before conversion whatever the app does.

-- ── header columns ───────────────────────────────────────────────────────────
ALTER TABLE purchase_requests
    ADD COLUMN version                      INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN business_purpose             TEXT NOT NULL DEFAULT '',
    ADD COLUMN cost_center                  TEXT NOT NULL DEFAULT '',
    ADD COLUMN project_ref                  TEXT NOT NULL DEFAULT '',
    ADD COLUMN budget_ref                   TEXT NOT NULL DEFAULT '',
    ADD COLUMN preferred_supplier_ref       TEXT NOT NULL DEFAULT '',
    ADD COLUMN required_date                DATE,
    ADD COLUMN attachment_refs              JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN expires_at                   TIMESTAMP WITH TIME ZONE,
    ADD COLUMN submitted_by_principal_id    VARCHAR(255),
    ADD COLUMN submitted_at                 TIMESTAMP WITH TIME ZONE,
    ADD COLUMN last_amended_by_principal_id VARCHAR(255),
    ADD COLUMN budget_decision              VARCHAR(20) NOT NULL DEFAULT 'NOT_CHECKED',
    ADD COLUMN budget_basis                 TEXT NOT NULL DEFAULT '',
    ADD COLUMN approval_invalidated_count   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN cancelled_by_principal_id    VARCHAR(255),
    ADD COLUMN cancelled_at                 TIMESTAMP WITH TIME ZONE,
    ADD COLUMN cancellation_reason          TEXT,
    ADD COLUMN converted_purchase_order_id  UUID,
    ADD COLUMN converted_by_principal_id    VARCHAR(255),
    ADD COLUMN converted_at                 TIMESTAMP WITH TIME ZONE,
    ADD COLUMN updated_at                   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW();

-- Legacy rows were created directly in the approvable state.
UPDATE purchase_requests SET status = 'PENDING_APPROVAL' WHERE status = 'PENDING';

ALTER TABLE purchase_requests
    ADD CONSTRAINT purchase_requests_status_known CHECK (status IN
        ('DRAFT','PENDING_APPROVAL','APPROVED','REJECTED','CONVERTED','CANCELLED','EXPIRED')),
    ADD CONSTRAINT purchase_requests_budget_decision_known CHECK (budget_decision IN
        ('NOT_CHECKED','NOT_REQUIRED','ALLOWED','BLOCKED')),
    -- A converted requisition must name the PO it produced.
    ADD CONSTRAINT purchase_requests_converted_has_po CHECK (status <> 'CONVERTED' OR converted_purchase_order_id IS NOT NULL);

-- A requisition converts to at most one PO, and a PO is claimed by one requisition.
CREATE UNIQUE INDEX uq_purchase_requests_converted_po
    ON purchase_requests (tenant_id, converted_purchase_order_id)
    WHERE converted_purchase_order_id IS NOT NULL;

-- ── lines ────────────────────────────────────────────────────────────────────
CREATE TABLE purchase_request_lines (
    line_id                 UUID PRIMARY KEY,
    request_id              UUID NOT NULL REFERENCES purchase_requests(request_id),
    tenant_id               UUID NOT NULL,
    line_number             INTEGER NOT NULL,
    item_ref                TEXT NOT NULL DEFAULT '',
    description             TEXT NOT NULL DEFAULT '',
    category                TEXT NOT NULL DEFAULT '',
    quantity                NUMERIC(18,4) NOT NULL CHECK (quantity > 0),
    unit_of_measure         TEXT NOT NULL DEFAULT '',
    amount                  NUMERIC(18,2) NOT NULL CHECK (amount > 0),
    currency_code           VARCHAR(3) NOT NULL,
    required_date           DATE,
    cost_center             TEXT NOT NULL DEFAULT '',
    project_ref             TEXT NOT NULL DEFAULT '',
    budget_ref              TEXT NOT NULL DEFAULT '',
    preferred_supplier_ref  TEXT NOT NULL DEFAULT '',
    attachment_refs         JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (request_id, line_number)
);
CREATE INDEX idx_purchase_request_lines_request ON purchase_request_lines (request_id);
CREATE INDEX idx_purchase_request_lines_tenant ON purchase_request_lines (tenant_id);

-- Backfill: every legacy header becomes a single legacy line carrying its amount.
INSERT INTO purchase_request_lines (line_id, request_id, tenant_id, line_number, description, category, quantity, amount, currency_code)
SELECT gen_random_uuid(), request_id, tenant_id, 1, description, 'GENERAL', 1, amount, currency_code
FROM purchase_requests;

ALTER TABLE purchase_request_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE purchase_request_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purchase_request_lines
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ── history (versioned evidence) ─────────────────────────────────────────────
CREATE TABLE purchase_request_history (
    history_id     UUID PRIMARY KEY,
    request_id     UUID NOT NULL REFERENCES purchase_requests(request_id),
    tenant_id      UUID NOT NULL,
    version        INTEGER NOT NULL,
    action         VARCHAR(40) NOT NULL,
    from_status    VARCHAR(20),
    to_status      VARCHAR(20) NOT NULL,
    actor          VARCHAR(255) NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    -- Versioned lines/coding snapshot, budget/policy response, threshold and
    -- conversion link as they stood at this step (spec §5 evidence/lineage).
    details        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_purchase_request_history_request ON purchase_request_history (request_id, created_at);

ALTER TABLE purchase_request_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE purchase_request_history FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON purchase_request_history
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ── immutability (the runtime is a superuser, so triggers are the enforcement) ─

CREATE OR REPLACE FUNCTION reject_pr_history_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'purchase_request_history is append-only (% rejected)', TG_OP;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pr_history_append_only
    BEFORE UPDATE OR DELETE ON purchase_request_history
    FOR EACH ROW EXECUTE FUNCTION reject_pr_history_mutation();

-- Header guard. Material objects are never hard-deleted; terminal states are
-- frozen; and an APPROVED requisition's content cannot change before
-- conversion — the only exits are conversion, cancellation, expiry, or an
-- amendment that drops it back to DRAFT (which invalidates the approval).
CREATE OR REPLACE FUNCTION guard_purchase_request() RETURNS TRIGGER AS $$
DECLARE
    content_changed BOOLEAN;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'purchase_requests rows are never deleted; use CancelRequisition';
    END IF;

    IF OLD.status IN ('CONVERTED','CANCELLED','REJECTED','EXPIRED') THEN
        RAISE EXCEPTION 'purchase request % is % and can no longer change', OLD.request_id, OLD.status;
    END IF;

    content_changed :=
           NEW.amount                  IS DISTINCT FROM OLD.amount
        OR NEW.currency_code           IS DISTINCT FROM OLD.currency_code
        OR NEW.description             IS DISTINCT FROM OLD.description
        OR NEW.business_purpose        IS DISTINCT FROM OLD.business_purpose
        OR NEW.cost_center             IS DISTINCT FROM OLD.cost_center
        OR NEW.project_ref             IS DISTINCT FROM OLD.project_ref
        OR NEW.budget_ref              IS DISTINCT FROM OLD.budget_ref
        OR NEW.preferred_supplier_ref  IS DISTINCT FROM OLD.preferred_supplier_ref
        OR NEW.required_date           IS DISTINCT FROM OLD.required_date
        OR NEW.attachment_refs         IS DISTINCT FROM OLD.attachment_refs
        OR NEW.legal_entity_id         IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.tenant_id               IS DISTINCT FROM OLD.tenant_id
        OR NEW.requested_by_principal_id IS DISTINCT FROM OLD.requested_by_principal_id;

    IF OLD.status IN ('APPROVED','PENDING_APPROVAL') AND content_changed AND NEW.status <> 'DRAFT' THEN
        RAISE EXCEPTION 'purchase request % is % — its content cannot change; amending drops it back to DRAFT and invalidates the approval', OLD.request_id, OLD.status;
    END IF;

    IF OLD.status = 'APPROVED' AND NEW.status NOT IN ('APPROVED','DRAFT','CONVERTED','CANCELLED','EXPIRED') THEN
        RAISE EXCEPTION 'illegal transition APPROVED -> %', NEW.status;
    END IF;
    IF OLD.status = 'DRAFT' AND NEW.status NOT IN ('DRAFT','PENDING_APPROVAL','CANCELLED') THEN
        RAISE EXCEPTION 'illegal transition DRAFT -> %', NEW.status;
    END IF;
    IF OLD.status = 'PENDING_APPROVAL' AND NEW.status NOT IN ('PENDING_APPROVAL','APPROVED','REJECTED','DRAFT','CANCELLED','EXPIRED') THEN
        RAISE EXCEPTION 'illegal transition PENDING_APPROVAL -> %', NEW.status;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_guard_purchase_request
    BEFORE UPDATE OR DELETE ON purchase_requests
    FOR EACH ROW EXECUTE FUNCTION guard_purchase_request();

-- Lines may only change while the parent is DRAFT.
CREATE OR REPLACE FUNCTION guard_purchase_request_line() RETURNS TRIGGER AS $$
DECLARE
    parent_status VARCHAR(20);
    parent_id     UUID;
BEGIN
    IF TG_OP = 'DELETE' THEN
        parent_id := OLD.request_id;
    ELSE
        parent_id := NEW.request_id;
    END IF;
    SELECT status INTO parent_status FROM purchase_requests WHERE request_id = parent_id;
    IF parent_status IS DISTINCT FROM 'DRAFT' THEN
        RAISE EXCEPTION 'lines of purchase request % cannot change while it is % (only DRAFT lines are editable)', parent_id, parent_status;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_guard_purchase_request_line
    BEFORE INSERT OR UPDATE OR DELETE ON purchase_request_lines
    FOR EACH ROW EXECUTE FUNCTION guard_purchase_request_line();
