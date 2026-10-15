-- Migration: 000300_ar03_sales_orders.up.sql
-- AR-03 Sales Order (spec ZS-SVC-C-001 section 6). COMMERCIAL SOURCE ONLY: nothing here
-- queues an accounting posting or creates a receivable.
--   ar03_order_counters        per (tenant, legal entity) order number sequence
--   ar03_sales_orders          stable order header + lifecycle / hold / cancel facts
--   ar03_order_versions        immutable content versions (governed amendments)
--   ar03_order_lines           current lines with invoiced / remaining position (guarded by triggers)
--   ar03_billing_milestones    billable milestones, UNIQUE external ref per (tenant, order)
--   ar03_invoiced_ledger       append-only (milestone, invoice) ledger: idempotency + duplicate-billing backstop
--   ar03_order_approvals       approvals bound to a content hash (append-only, SoD in the data)
--   ar03_credit_exceptions     approved credit exceptions (append-only, SoD in the data)
--   ar03_order_history         append-only evidence of every command

CREATE TABLE IF NOT EXISTS ar03_order_counters (
    tenant_id       UUID    NOT NULL,
    legal_entity_id UUID    NOT NULL,
    last_no         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, legal_entity_id)
);

CREATE TABLE IF NOT EXISTS ar03_sales_orders (
    order_id              UUID PRIMARY KEY,
    tenant_id             UUID          NOT NULL,
    legal_entity_id       UUID          NOT NULL,
    order_number          VARCHAR(32)   NOT NULL,
    customer_party_id     UUID          NOT NULL,
    currency              VARCHAR(3)    NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    state                 VARCHAR(16)   NOT NULL CHECK (state IN ('DRAFT','PENDING_APPROVAL','APPROVED','CONFIRMED','IN_FULFILLMENT',
                                                                  'PARTIALLY_BILLED','FULLY_BILLED','CLOSED','CANCELLED')),
    hold_active           BOOLEAN       NOT NULL DEFAULT FALSE,
    hold_kind             VARCHAR(16)   CHECK (hold_kind IN ('CREDIT','COMMERCIAL','FULFILLMENT','OTHER')),
    hold_reason           VARCHAR(128),
    hold_evidence_id      VARCHAR(255),
    held_by               VARCHAR(255),
    held_at               TIMESTAMPTZ,
    customer_po_ref       VARCHAR(255)  NOT NULL DEFAULT '',
    source_quote_ref      VARCHAR(255),
    source_ref            VARCHAR(255),
    net_total             NUMERIC(20,4) NOT NULL CHECK (net_total >= 0),
    discount_percent      NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (discount_percent >= 0 AND discount_percent <= 100),
    current_version_no    INTEGER       NOT NULL CHECK (current_version_no >= 1),
    version               INTEGER       NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_by            VARCHAR(255)  NOT NULL,
    submitted_by          VARCHAR(255),
    approved_version_no   INTEGER,
    approved_content_hash VARCHAR(64),
    approved_by           VARCHAR(255),
    approved_at           TIMESTAMPTZ,
    credit_exception_id   UUID,
    confirmed_by          VARCHAR(255),
    confirmed_at          TIMESTAMPTZ,
    cancel_reason         VARCHAR(128),
    cancelled_by          VARCHAR(255),
    cancelled_at          TIMESTAMPTZ,
    close_reason          VARCHAR(128),
    closed_by             VARCHAR(255),
    closed_at             TIMESTAMPTZ,
    created_at            TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, legal_entity_id, order_number),
    CONSTRAINT ar03_state_requires_approval CHECK (state NOT IN ('APPROVED','CONFIRMED','IN_FULFILLMENT','PARTIALLY_BILLED','FULLY_BILLED','CLOSED')
        OR approved_content_hash IS NOT NULL),
    CONSTRAINT ar03_confirmed_requires_confirmer CHECK (state NOT IN ('CONFIRMED','IN_FULFILLMENT','PARTIALLY_BILLED','FULLY_BILLED')
        OR (confirmed_by IS NOT NULL AND confirmed_at IS NOT NULL)),
    CONSTRAINT ar03_cancel_requires_reason CHECK (state <> 'CANCELLED' OR (cancel_reason IS NOT NULL AND cancelled_by IS NOT NULL)),
    CONSTRAINT ar03_closed_requires_closer CHECK (state <> 'CLOSED' OR (closed_by IS NOT NULL AND closed_at IS NOT NULL)),
    CONSTRAINT ar03_hold_requires_facts CHECK (NOT hold_active
        OR (hold_kind IS NOT NULL AND hold_reason IS NOT NULL AND hold_evidence_id IS NOT NULL AND held_by IS NOT NULL)),
    CONSTRAINT ar03_hold_only_when_live CHECK (NOT hold_active
        OR state IN ('PENDING_APPROVAL','APPROVED','CONFIRMED','IN_FULFILLMENT','PARTIALLY_BILLED'))
);
-- A source reference (quote:<id>:v<n>) creates at most one order per tenant.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar03_order_source_ref ON ar03_sales_orders (tenant_id, source_ref) WHERE source_ref IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ar03_orders_customer ON ar03_sales_orders (tenant_id, legal_entity_id, customer_party_id);

-- Identity never changes; closed and cancelled orders are frozen; nothing is deleted.
CREATE OR REPLACE FUNCTION ar03_guard_orders() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR03_ORDER_NOT_DELETABLE: a sales order is never deleted';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.customer_party_id <> OLD.customer_party_id
       OR NEW.currency <> OLD.currency OR NEW.order_number <> OLD.order_number OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'AR03_ORDER_IDENTITY_IMMUTABLE: tenant, entity, customer, currency, number and creator never change';
    END IF;
    IF OLD.state IN ('CLOSED','CANCELLED') THEN
        RAISE EXCEPTION 'AR03_ORDER_TERMINAL: a % order is frozen', OLD.state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar03_orders_guard ON ar03_sales_orders;
CREATE TRIGGER ar03_orders_guard BEFORE UPDATE OR DELETE ON ar03_sales_orders
    FOR EACH ROW EXECUTE FUNCTION ar03_guard_orders();

CREATE TABLE IF NOT EXISTS ar03_order_versions (
    order_id            UUID          NOT NULL REFERENCES ar03_sales_orders (order_id),
    version_no          INTEGER       NOT NULL CHECK (version_no >= 1),
    tenant_id           UUID          NOT NULL,
    legal_entity_id     UUID          NOT NULL,
    currency            VARCHAR(3)    NOT NULL,
    customer_po_ref     VARCHAR(255)  NOT NULL DEFAULT '',
    terms               TEXT          NOT NULL DEFAULT '',
    price_source_ref    VARCHAR(255)  NOT NULL DEFAULT '',
    catalog_version_ref VARCHAR(255)  NOT NULL DEFAULT '',
    discount_percent    NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (discount_percent >= 0 AND discount_percent <= 100),
    subtotal            NUMERIC(20,4) NOT NULL CHECK (subtotal >= 0),
    discount_amount     NUMERIC(20,4) NOT NULL CHECK (discount_amount >= 0),
    net_total           NUMERIC(20,4) NOT NULL CHECK (net_total >= 0),
    content_hash        VARCHAR(64)   NOT NULL,
    lines               JSONB         NOT NULL,
    change_kind         VARCHAR(16)   NOT NULL CHECK (change_kind IN ('CREATE','AMEND')),
    reason_code         VARCHAR(128),
    evidence_id         VARCHAR(255),
    created_by          VARCHAR(255)  NOT NULL,
    created_at          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    PRIMARY KEY (order_id, version_no),
    CONSTRAINT ar03_totals_consistent CHECK (net_total = subtotal - discount_amount)
);

CREATE OR REPLACE FUNCTION ar03_check_version_seq() RETURNS trigger AS $$
DECLARE
    mx INTEGER;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.order_id::text, 300));
    SELECT max(version_no) INTO mx FROM ar03_order_versions WHERE order_id = NEW.order_id;
    IF (mx IS NULL AND NEW.version_no <> 1) OR (mx IS NOT NULL AND NEW.version_no <> mx + 1) THEN
        RAISE EXCEPTION 'AR03_VERSION_SEQUENCE: version_no % is not the next version', NEW.version_no;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar03_versions_seq ON ar03_order_versions;
CREATE TRIGGER ar03_versions_seq BEFORE INSERT ON ar03_order_versions
    FOR EACH ROW EXECUTE FUNCTION ar03_check_version_seq();
DROP TRIGGER IF EXISTS ar03_versions_immutable ON ar03_order_versions;
CREATE TRIGGER ar03_versions_immutable BEFORE UPDATE OR DELETE ON ar03_order_versions
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

-- Current lines. ordered_qty / unit_price / line_net change only inside a governed
-- amendment (the command sets ar03.amending to the order id for its transaction);
-- invoiced quantities and amounts only ever grow; remaining can never go negative.
CREATE TABLE IF NOT EXISTS ar03_order_lines (
    order_id         UUID          NOT NULL REFERENCES ar03_sales_orders (order_id),
    line_no          INTEGER       NOT NULL CHECK (line_no >= 1),
    tenant_id        UUID          NOT NULL,
    description      VARCHAR(500)  NOT NULL,
    product_ref      VARCHAR(255)  NOT NULL DEFAULT '',
    ordered_qty      NUMERIC(20,4) NOT NULL CHECK (ordered_qty > 0),
    unit_price       NUMERIC(20,4) NOT NULL CHECK (unit_price >= 0),
    list_net         NUMERIC(20,4) NOT NULL CHECK (list_net >= 0),
    discount_amount  NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (discount_amount >= 0),
    line_net         NUMERIC(20,4) NOT NULL CHECK (line_net >= 0),
    invoiced_qty     NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (invoiced_qty >= 0),
    invoiced_amount  NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (invoiced_amount >= 0),
    remaining_qty    NUMERIC(20,4) GENERATED ALWAYS AS (ordered_qty - invoiced_qty) STORED,
    remaining_amount NUMERIC(20,4) GENERATED ALWAYS AS (line_net - invoiced_amount) STORED,
    PRIMARY KEY (order_id, line_no),
    CONSTRAINT ar03_line_net_consistent CHECK (line_net = list_net - discount_amount),
    CONSTRAINT ar03_invoiced_within_ordered CHECK (invoiced_qty <= ordered_qty AND invoiced_amount <= line_net),
    CONSTRAINT ar03_remaining_non_negative CHECK (remaining_qty >= 0 AND remaining_amount >= 0)
);

CREATE OR REPLACE FUNCTION ar03_guard_lines() RETURNS trigger AS $$
DECLARE
    amending TEXT := COALESCE(current_setting('ar03.amending', true), '');
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF amending <> NEW.order_id::text THEN
            RAISE EXCEPTION 'AR03_LINE_AMENDMENT_REQUIRED: order lines are created only by a governed command';
        END IF;
        RETURN NEW;
    ELSIF TG_OP = 'DELETE' THEN
        IF OLD.invoiced_qty > 0 OR OLD.invoiced_amount > 0 THEN
            RAISE EXCEPTION 'AR03_BILLED_LINE_IMMUTABLE: a billed line cannot be removed';
        END IF;
        IF amending <> OLD.order_id::text THEN
            RAISE EXCEPTION 'AR03_LINE_AMENDMENT_REQUIRED: order lines change only through a governed amendment';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.order_id <> OLD.order_id OR NEW.line_no <> OLD.line_no OR NEW.tenant_id <> OLD.tenant_id THEN
        RAISE EXCEPTION 'AR03_LINE_IDENTITY_IMMUTABLE: a line keeps its order and number';
    END IF;
    IF NEW.invoiced_qty < OLD.invoiced_qty OR NEW.invoiced_amount < OLD.invoiced_amount THEN
        RAISE EXCEPTION 'AR03_INVOICED_NOT_REDUCIBLE: invoiced quantity and amount only grow';
    END IF;
    IF (NEW.description, NEW.product_ref, NEW.ordered_qty, NEW.unit_price, NEW.list_net, NEW.discount_amount, NEW.line_net)
       IS DISTINCT FROM (OLD.description, OLD.product_ref, OLD.ordered_qty, OLD.unit_price, OLD.list_net, OLD.discount_amount, OLD.line_net)
       AND amending <> NEW.order_id::text THEN
        RAISE EXCEPTION 'AR03_LINE_AMENDMENT_REQUIRED: order lines change only through a governed amendment';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar03_lines_guard ON ar03_order_lines;
CREATE TRIGGER ar03_lines_guard BEFORE INSERT OR UPDATE OR DELETE ON ar03_order_lines
    FOR EACH ROW EXECUTE FUNCTION ar03_guard_lines();

CREATE TABLE IF NOT EXISTS ar03_billing_milestones (
    milestone_id             UUID PRIMARY KEY,
    tenant_id                UUID          NOT NULL,
    legal_entity_id          UUID          NOT NULL,
    order_id                 UUID          NOT NULL,
    external_ref             VARCHAR(255)  NOT NULL CHECK (length(btrim(external_ref)) > 0),
    line_no                  INTEGER       NOT NULL,
    description              VARCHAR(500)  NOT NULL DEFAULT '',
    scheduled_qty            NUMERIC(20,4) NOT NULL CHECK (scheduled_qty >= 0),
    scheduled_amount         NUMERIC(20,4) NOT NULL CHECK (scheduled_amount > 0),
    state                    VARCHAR(16)   NOT NULL CHECK (state IN ('RECORDED','INVOICED','VOIDED')),
    -- Fulfillment evidence is owned by the operational domain; only its reference is recorded here.
    fulfillment_source       VARCHAR(64)   NOT NULL CHECK (length(btrim(fulfillment_source)) > 0),
    fulfillment_evidence_ref VARCHAR(255)  NOT NULL CHECK (length(btrim(fulfillment_evidence_ref)) > 0),
    recorded_by              VARCHAR(255)  NOT NULL,
    recorded_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    invoice_id               VARCHAR(255),
    invoiced_net             NUMERIC(20,4),
    invoiced_at              TIMESTAMPTZ,
    void_reason              VARCHAR(128),
    voided_at                TIMESTAMPTZ,
    FOREIGN KEY (order_id, line_no) REFERENCES ar03_order_lines (order_id, line_no),
    -- A duplicate milestone can never create a second invoice eligibility.
    CONSTRAINT uq_ar03_milestone_ref UNIQUE (tenant_id, order_id, external_ref),
    CONSTRAINT ar03_milestone_invoiced_facts CHECK (state <> 'INVOICED' OR (invoice_id IS NOT NULL AND invoiced_net = scheduled_amount AND invoiced_at IS NOT NULL)),
    CONSTRAINT ar03_milestone_voided_facts CHECK (state <> 'VOIDED' OR (void_reason IS NOT NULL AND voided_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_ar03_milestones_order ON ar03_billing_milestones (tenant_id, order_id);

CREATE OR REPLACE FUNCTION ar03_guard_milestones() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR03_MILESTONE_NOT_DELETABLE: a billing milestone is never deleted';
    END IF;
    IF OLD.state <> 'RECORDED' THEN
        RAISE EXCEPTION 'AR03_MILESTONE_FINAL: a % milestone cannot change', OLD.state;
    END IF;
    IF NEW.state NOT IN ('INVOICED','VOIDED') OR NEW.milestone_id <> OLD.milestone_id OR NEW.order_id <> OLD.order_id
       OR NEW.external_ref <> OLD.external_ref OR NEW.line_no <> OLD.line_no OR NEW.scheduled_qty <> OLD.scheduled_qty
       OR NEW.scheduled_amount <> OLD.scheduled_amount OR NEW.fulfillment_evidence_ref <> OLD.fulfillment_evidence_ref
       OR NEW.fulfillment_source <> OLD.fulfillment_source THEN
        RAISE EXCEPTION 'AR03_MILESTONE_IMMUTABLE: a recorded milestone only moves to INVOICED or VOIDED';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar03_milestones_guard ON ar03_billing_milestones;
CREATE TRIGGER ar03_milestones_guard BEFORE UPDATE OR DELETE ON ar03_billing_milestones
    FOR EACH ROW EXECUTE FUNCTION ar03_guard_milestones();

-- One row per invoice recorded against a milestone. (milestone, invoice) makes
-- RecordInvoiced idempotent; (tenant, milestone) alone makes a second invoice for the
-- same milestone impossible even if the application check were bypassed.
CREATE TABLE IF NOT EXISTS ar03_invoiced_ledger (
    ledger_id       UUID PRIMARY KEY,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    order_id        UUID          NOT NULL REFERENCES ar03_sales_orders (order_id),
    milestone_id    UUID          NOT NULL REFERENCES ar03_billing_milestones (milestone_id),
    invoice_id      VARCHAR(255)  NOT NULL CHECK (length(btrim(invoice_id)) > 0),
    net             NUMERIC(20,4) NOT NULL CHECK (net > 0),
    recorded_by     VARCHAR(255)  NOT NULL,
    recorded_at     TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_ar03_ledger_milestone_invoice UNIQUE (tenant_id, milestone_id, invoice_id),
    CONSTRAINT uq_ar03_ledger_milestone UNIQUE (tenant_id, milestone_id)
);
DROP TRIGGER IF EXISTS ar03_ledger_append_only ON ar03_invoiced_ledger;
CREATE TRIGGER ar03_ledger_append_only BEFORE UPDATE OR DELETE ON ar03_invoiced_ledger
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

CREATE TABLE IF NOT EXISTS ar03_order_approvals (
    approval_id     UUID PRIMARY KEY,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    order_id        UUID          NOT NULL,
    version_no      INTEGER       NOT NULL,
    content_hash    VARCHAR(64)   NOT NULL,
    net_total       NUMERIC(20,4) NOT NULL,
    sod_required    BOOLEAN       NOT NULL,
    created_by      VARCHAR(255)  NOT NULL,
    submitted_by    VARCHAR(255)  NOT NULL,
    approved_by     VARCHAR(255)  NOT NULL,
    approved_at     TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    FOREIGN KEY (order_id, version_no) REFERENCES ar03_order_versions (order_id, version_no),
    CONSTRAINT ar03_approval_sod CHECK (NOT sod_required OR (approved_by <> created_by AND approved_by <> submitted_by))
);
CREATE INDEX IF NOT EXISTS idx_ar03_approvals_order ON ar03_order_approvals (order_id, version_no);
DROP TRIGGER IF EXISTS ar03_approvals_append_only ON ar03_order_approvals;
CREATE TRIGGER ar03_approvals_append_only BEFORE UPDATE OR DELETE ON ar03_order_approvals
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

CREATE TABLE IF NOT EXISTS ar03_credit_exceptions (
    exception_id    UUID PRIMARY KEY,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    order_id        UUID          NOT NULL,
    version_no      INTEGER       NOT NULL,
    content_hash    VARCHAR(64)   NOT NULL,
    order_net       NUMERIC(20,4) NOT NULL,
    reason_code     VARCHAR(128)  NOT NULL CHECK (length(btrim(reason_code)) > 0),
    evidence_id     VARCHAR(255)  NOT NULL CHECK (length(btrim(evidence_id)) > 0),
    created_by      VARCHAR(255)  NOT NULL,
    submitted_by    VARCHAR(255),
    approved_by     VARCHAR(255)  NOT NULL,
    recorded_at     TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    FOREIGN KEY (order_id, version_no) REFERENCES ar03_order_versions (order_id, version_no),
    CONSTRAINT ar03_exception_sod CHECK (approved_by <> created_by AND approved_by <> COALESCE(submitted_by, ''))
);
CREATE INDEX IF NOT EXISTS idx_ar03_exceptions_order ON ar03_credit_exceptions (order_id);
DROP TRIGGER IF EXISTS ar03_exceptions_append_only ON ar03_credit_exceptions;
CREATE TRIGGER ar03_exceptions_append_only BEFORE UPDATE OR DELETE ON ar03_credit_exceptions
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

CREATE TABLE IF NOT EXISTS ar03_order_history (
    history_id      UUID PRIMARY KEY,
    tenant_id       UUID         NOT NULL,
    legal_entity_id UUID         NOT NULL,
    order_id        UUID         NOT NULL REFERENCES ar03_sales_orders (order_id),
    order_version   INTEGER      NOT NULL,
    version_no      INTEGER      NOT NULL,
    command         VARCHAR(64)  NOT NULL,
    state_after     VARCHAR(16)  NOT NULL,
    actor_id        VARCHAR(255) NOT NULL,
    approver_id     VARCHAR(255),
    reason_code     VARCHAR(128),
    evidence_id     VARCHAR(255),
    content_hash    VARCHAR(64),
    prior_values    JSONB,
    new_values      JSONB,
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    recorded_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (order_id, order_version)
);
DROP TRIGGER IF EXISTS ar03_history_append_only ON ar03_order_history;
CREATE TRIGGER ar03_history_append_only BEFORE UPDATE OR DELETE ON ar03_order_history
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar03_order_counters','ar03_sales_orders','ar03_order_versions','ar03_order_lines','ar03_billing_milestones',
                             'ar03_invoiced_ledger','ar03_order_approvals','ar03_credit_exceptions','ar03_order_history'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
