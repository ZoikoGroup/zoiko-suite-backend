-- Migration: 000010_ap06_invoice_matching.up.sql
--
-- AP-06 Invoice Matching (ZS-SVC-D-001), inside accounts-payable-svc.
--
--   * match_policy_versions   versioned two-/three-way tolerance policy (append-only).
--   * invoice_match_runs      one row per match run: frozen policy snapshot, PO revision,
--                             input hash, totals. A run is immutable once written; the
--                             only change permitted is being SUPERSEDED (set once).
--   * invoice_match_lines     line-level match facts of a run (append-only).
--   * invoice_match_exceptions exception workflow rows. Identity is immutable; only the
--                             resolution columns move, forward only.
--
-- plus two invoice-level guards so the control holds for EVERY write path (legacy
-- /approve, v2 commands, raw SQL), not just the handler:
--   * chk_vi_match_cleared: match_cleared requires a run id and a cleared match state.
--   * vendor_invoices_match_gate: a PO-backed invoice cannot become APPROVED unless its
--     match is cleared.
--
-- Tenant tables are FORCE-RLS (same predicate as migration 000005); the service also
-- filters on tenant explicitly.

CREATE TABLE IF NOT EXISTS match_policy_versions (
    tenant_id             UUID        NOT NULL,
    legal_entity_id       UUID        NOT NULL,
    policy_version        INTEGER     NOT NULL CHECK (policy_version > 0),
    mode                  VARCHAR(16) NOT NULL CHECK (mode IN ('TWO_WAY', 'THREE_WAY')),
    qty_tolerance_pct     NUMERIC(9,4)  NOT NULL DEFAULT 0 CHECK (qty_tolerance_pct >= 0),
    price_tolerance_pct   NUMERIC(9,4)  NOT NULL DEFAULT 0 CHECK (price_tolerance_pct >= 0),
    amount_tolerance_abs  NUMERIC(18,2) NOT NULL DEFAULT 0 CHECK (amount_tolerance_abs >= 0),
    reason                TEXT        NOT NULL DEFAULT '',
    created_by            VARCHAR(255) NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, legal_entity_id, policy_version)
);

CREATE TABLE IF NOT EXISTS invoice_match_runs (
    run_id               UUID        PRIMARY KEY,
    tenant_id            UUID        NOT NULL,
    legal_entity_id      UUID        NOT NULL,
    invoice_id           UUID        NOT NULL REFERENCES vendor_invoices(invoice_id),
    run_number           INTEGER     NOT NULL CHECK (run_number > 0),
    result               VARCHAR(20) NOT NULL CHECK (result IN ('MATCHED','WITHIN_TOLERANCE','EXCEPTION','INCOMPLETE')),
    mode                 VARCHAR(16) NOT NULL CHECK (mode IN ('TWO_WAY','THREE_WAY')),
    policy_version       INTEGER     NOT NULL CHECK (policy_version >= 0),   -- 0 = built-in strict default
    policy_snapshot      JSONB       NOT NULL,
    purchase_order_id    TEXT        NOT NULL,
    po_revision          INTEGER,
    invoice_version      INTEGER     NOT NULL,
    input_hash           VARCHAR(64) NOT NULL,
    totals               JSONB       NOT NULL,
    requested_by         VARCHAR(255) NOT NULL,
    correlation_id       VARCHAR(255) NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    superseded_at        TIMESTAMPTZ,
    superseded_by_run_id UUID,
    supersede_reason     TEXT,
    UNIQUE (tenant_id, invoice_id, run_number)
);
-- At most one live (non-superseded) run per invoice.
CREATE UNIQUE INDEX IF NOT EXISTS uq_invoice_match_runs_current ON invoice_match_runs (tenant_id, invoice_id) WHERE superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_invoice_match_runs_invoice ON invoice_match_runs (tenant_id, invoice_id, run_number DESC);

CREATE TABLE IF NOT EXISTS invoice_match_lines (
    line_result_id     UUID        PRIMARY KEY,
    tenant_id          UUID        NOT NULL,
    run_id             UUID        NOT NULL REFERENCES invoice_match_runs(run_id),
    invoice_id         UUID        NOT NULL,
    invoice_line_id    UUID,
    line_number        INTEGER     NOT NULL,
    po_line_id         TEXT,
    result             VARCHAR(20) NOT NULL CHECK (result IN ('MATCHED','WITHIN_TOLERANCE','EXCEPTION','INCOMPLETE')),
    detail             JSONB       NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_invoice_match_lines_run ON invoice_match_lines (tenant_id, run_id, line_number);

CREATE TABLE IF NOT EXISTS invoice_match_exceptions (
    exception_id       UUID        PRIMARY KEY,
    tenant_id          UUID        NOT NULL,
    legal_entity_id    UUID        NOT NULL,
    run_id             UUID        NOT NULL REFERENCES invoice_match_runs(run_id),
    invoice_id         UUID        NOT NULL,
    invoice_line_id    UUID,
    po_line_id         TEXT,
    category           VARCHAR(40) NOT NULL,
    class              VARCHAR(12) NOT NULL CHECK (class IN ('INCOMPLETE','VARIANCE')),
    waivable           BOOLEAN     NOT NULL,
    expected           NUMERIC(18,4),
    actual             NUMERIC(18,4),
    difference         NUMERIC(18,4),
    detail             TEXT        NOT NULL DEFAULT '',
    status             VARCHAR(20) NOT NULL DEFAULT 'OPEN'
                       CHECK (status IN ('OPEN','ACKNOWLEDGED','ROUTED','VARIANCE_APPROVED')),
    acknowledged_by    VARCHAR(255),
    acknowledged_at    TIMESTAMPTZ,
    routed_to          VARCHAR(255),
    routed_by          VARCHAR(255),
    routed_at          TIMESTAMPTZ,
    route_reason       TEXT,
    resolved_by        VARCHAR(255),
    resolved_at        TIMESTAMPTZ,
    resolution_reason  TEXT,
    resolution_ref     TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Only a variance can be waived; missing evidence is fixed, never waived.
    CONSTRAINT chk_match_exc_waive CHECK (status <> 'VARIANCE_APPROVED' OR (waivable AND class = 'VARIANCE')),
    CONSTRAINT chk_match_exc_resolved CHECK (status <> 'VARIANCE_APPROVED' OR (resolved_by IS NOT NULL AND resolved_at IS NOT NULL AND COALESCE(resolution_reason, '') <> ''))
);
CREATE INDEX IF NOT EXISTS idx_invoice_match_exceptions_run ON invoice_match_exceptions (tenant_id, run_id);
CREATE INDEX IF NOT EXISTS idx_invoice_match_exceptions_entity ON invoice_match_exceptions (tenant_id, legal_entity_id, status);

-- ── RLS ─────────────────────────────────────────────────────────────────────
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['match_policy_versions','invoice_match_runs','invoice_match_lines','invoice_match_exceptions'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;

-- ── Immutability ────────────────────────────────────────────────────────────
-- Policy versions and line facts are append-only (ap_append_only_guard, migration 000008).
DROP TRIGGER IF EXISTS trg_match_policy_append_only ON match_policy_versions;
CREATE TRIGGER trg_match_policy_append_only BEFORE UPDATE OR DELETE ON match_policy_versions
    FOR EACH ROW EXECUTE FUNCTION ap_append_only_guard();
DROP TRIGGER IF EXISTS trg_match_lines_append_only ON invoice_match_lines;
CREATE TRIGGER trg_match_lines_append_only BEFORE UPDATE OR DELETE ON invoice_match_lines
    FOR EACH ROW EXECUTE FUNCTION ap_append_only_guard();

-- A certified run is immutable: its frozen inputs and result never change. The one
-- permitted change is supersession, which is set once and never undone.
CREATE OR REPLACE FUNCTION invoice_match_runs_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invoice_match_runs rows are never deleted' USING ERRCODE = '23000';
    END IF;
    IF NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.invoice_id IS DISTINCT FROM OLD.invoice_id
        OR NEW.run_number IS DISTINCT FROM OLD.run_number
        OR NEW.result IS DISTINCT FROM OLD.result
        OR NEW.mode IS DISTINCT FROM OLD.mode
        OR NEW.policy_version IS DISTINCT FROM OLD.policy_version
        OR NEW.policy_snapshot IS DISTINCT FROM OLD.policy_snapshot
        OR NEW.purchase_order_id IS DISTINCT FROM OLD.purchase_order_id
        OR NEW.po_revision IS DISTINCT FROM OLD.po_revision
        OR NEW.invoice_version IS DISTINCT FROM OLD.invoice_version
        OR NEW.input_hash IS DISTINCT FROM OLD.input_hash
        OR NEW.totals IS DISTINCT FROM OLD.totals
        OR NEW.requested_by IS DISTINCT FROM OLD.requested_by
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'a match run is immutable once written (only supersession is permitted)' USING ERRCODE = '23000';
    END IF;
    IF OLD.superseded_at IS NOT NULL THEN
        RAISE EXCEPTION 'a superseded match run cannot change again' USING ERRCODE = '23000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_invoice_match_runs_guard ON invoice_match_runs;
CREATE TRIGGER trg_invoice_match_runs_guard BEFORE UPDATE OR DELETE ON invoice_match_runs
    FOR EACH ROW EXECUTE FUNCTION invoice_match_runs_guard();

-- Exception rows: identity and findings are immutable; resolution moves forward only,
-- on the live run only, and a variance is never approved by the principal who asked
-- for the run or who created the invoice (no self-waiver).
CREATE OR REPLACE FUNCTION invoice_match_exceptions_guard() RETURNS trigger AS $$
DECLARE
    run_requester TEXT;
    run_superseded TIMESTAMPTZ;
    inv_creator TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invoice_match_exceptions rows are never deleted' USING ERRCODE = '23000';
    END IF;
    IF NEW.exception_id IS DISTINCT FROM OLD.exception_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.invoice_id IS DISTINCT FROM OLD.invoice_id
        OR NEW.invoice_line_id IS DISTINCT FROM OLD.invoice_line_id
        OR NEW.po_line_id IS DISTINCT FROM OLD.po_line_id
        OR NEW.category IS DISTINCT FROM OLD.category
        OR NEW.class IS DISTINCT FROM OLD.class
        OR NEW.waivable IS DISTINCT FROM OLD.waivable
        OR NEW.expected IS DISTINCT FROM OLD.expected
        OR NEW.actual IS DISTINCT FROM OLD.actual
        OR NEW.difference IS DISTINCT FROM OLD.difference
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'a match exception''s findings are immutable' USING ERRCODE = '23000';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF OLD.status = 'VARIANCE_APPROVED' THEN
            RAISE EXCEPTION 'an approved variance is final' USING ERRCODE = '23000';
        END IF;
        IF OLD.status = 'ROUTED' AND NEW.status = 'OPEN' THEN
            RAISE EXCEPTION 'a routed exception cannot go back to OPEN' USING ERRCODE = '23000';
        END IF;
        IF OLD.status = 'ACKNOWLEDGED' AND NEW.status = 'OPEN' THEN
            RAISE EXCEPTION 'an acknowledged exception cannot go back to OPEN' USING ERRCODE = '23000';
        END IF;
        SELECT requested_by, superseded_at INTO run_requester, run_superseded
          FROM invoice_match_runs WHERE run_id = NEW.run_id;
        IF run_superseded IS NOT NULL THEN
            RAISE EXCEPTION 'the match run of this exception has been superseded' USING ERRCODE = '23000';
        END IF;
        IF NEW.status = 'VARIANCE_APPROVED' THEN
            SELECT created_by_principal_id INTO inv_creator FROM vendor_invoices WHERE invoice_id = NEW.invoice_id;
            IF NEW.resolved_by = run_requester OR NEW.resolved_by = inv_creator THEN
                RAISE EXCEPTION 'segregation of duties: the principal who ran the match or created the invoice cannot approve its variance'
                    USING ERRCODE = '23000';
            END IF;
        END IF;
    ELSIF OLD.status = 'VARIANCE_APPROVED' THEN
        RAISE EXCEPTION 'an approved variance is final' USING ERRCODE = '23000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_invoice_match_exceptions_guard ON invoice_match_exceptions;
CREATE TRIGGER trg_invoice_match_exceptions_guard BEFORE UPDATE OR DELETE ON invoice_match_exceptions
    FOR EACH ROW EXECUTE FUNCTION invoice_match_exceptions_guard();

-- ── Invoice-level guards ────────────────────────────────────────────────────
ALTER TABLE vendor_invoices DROP CONSTRAINT IF EXISTS chk_vi_match_cleared;
ALTER TABLE vendor_invoices ADD CONSTRAINT chk_vi_match_cleared
    CHECK (NOT match_cleared OR (match_run_id IS NOT NULL AND match_state IN ('MATCHED','WITHIN_TOLERANCE')));

CREATE OR REPLACE FUNCTION vendor_invoices_match_gate() RETURNS trigger AS $$
BEGIN
    IF NEW.approval_state = 'APPROVED' AND OLD.approval_state IS DISTINCT FROM 'APPROVED'
       AND NEW.document_type = 'INVOICE' AND NEW.purchase_order_id IS NOT NULL
       AND NEW.match_cleared IS NOT TRUE THEN
        RAISE EXCEPTION 'a PO-backed invoice cannot be approved until its AP-06 match is MATCHED, WITHIN_TOLERANCE or has approved variances'
            USING ERRCODE = '23000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_vendor_invoices_match_gate ON vendor_invoices;
CREATE TRIGGER trg_vendor_invoices_match_gate BEFORE UPDATE ON vendor_invoices
    FOR EACH ROW EXECUTE FUNCTION vendor_invoices_match_gate();
