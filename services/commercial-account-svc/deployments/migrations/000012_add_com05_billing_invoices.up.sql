-- 000012_add_com05_billing_invoices.up.sql
-- COM-05 Platform Commercial Billing, part 5a (ZS-SVC-Q-001 §4.5; COM-CTRL-001,
-- -019, -020, -021, -022; negative paths #01, #02, #20, #21, #23, #24, #42, #45,
-- #46, #47).
--
-- Scope of this part: BillingAccount, InvoiceCandidate, PlatformCommercialInvoice,
-- InvoiceLine — GenerateInvoiceCandidate / ApproveInvoice / IssueInvoice and their
-- reads. PaymentAttemptRef/CollectionState, CreditNote/refund/write-off/
-- OutstandingBalance and DunningCase/CommercialReconciliation are later parts of
-- this same wave.
--
-- Rating basis is deliberately narrow here: only RECURRING_FIXED/PER_UNIT
-- components of the subscription's effective SubscriptionVersion (COM-02) and
-- METERED components rated against a CERTIFIED/ADJUSTED UsageStatement
-- (COM-04) for the exact term being billed — the two rows §5's "Charge Basis"
-- table actually names ("Commercial basis", "Usage basis"). ONE_TIME and
-- DISCOUNT components are out of scope for this part.
--
-- Tax is a required, caller-supplied input (a jurisdiction code, a rate and an
-- amount), never computed here: COM-CTRL-020 forbids this service from owning
-- tax rule logic or reading tenant tax configuration, and no ZoikoSuite merchant
-- tax engine exists yet in this codebase to compute it. The mechanism (evidence
-- captured on generate, immutable once issued, invalidates approval if it would
-- differ on reissue) is real; the computation is external.
--
-- Custom SQLSTATE reused from prior waves: CP001 immutable/lifecycle violation.

-- ── Billing account (server-resolved context) ─────────────────────────────
--
-- One per organization: which ZoikoSuite selling entity invoices it, which
-- invoice numbering series and payment provider it uses. Set by platform
-- billing operations, never by the customer.
CREATE TABLE billing_accounts (
    billing_account_id        TEXT         PRIMARY KEY
        CHECK (billing_account_id ~ '^cba_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id            UUID         NOT NULL UNIQUE,
    selling_entity              VARCHAR(64)  NOT NULL CHECK (btrim(selling_entity) <> ''),
    billing_currency_code       CHAR(3)      NOT NULL CHECK (billing_currency_code ~ '^[A-Z]{3}$'),
    invoice_numbering_profile   VARCHAR(64)  NOT NULL CHECK (btrim(invoice_numbering_profile) <> ''),
    payment_provider_ref        VARCHAR(255) NOT NULL CHECK (btrim(payment_provider_ref) <> ''),
    status                       VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'CLOSED')),
    created_at                   TIMESTAMPTZ  NOT NULL,
    created_by_principal_id      VARCHAR(255) NOT NULL
);

-- reject_immutable_row() already exists (defined in migration 000010) and is
-- reused here as-is.

ALTER TABLE billing_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY billing_accounts_read ON billing_accounts FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY billing_accounts_seller_insert ON billing_accounts FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- ── Invoice numbering (seller plane only) ─────────────────────────────────
--
-- A gapless, sequential number per numbering profile, incremented under a
-- row lock inside the same transaction that issues the invoice it names
-- (COM-CTRL-021).
CREATE TABLE invoice_number_counters (
    numbering_profile   VARCHAR(64) PRIMARY KEY,
    next_number          BIGINT      NOT NULL DEFAULT 1 CHECK (next_number >= 1)
);

ALTER TABLE invoice_number_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_number_counters FORCE ROW LEVEL SECURITY;
CREATE POLICY invoice_counters_seller_only ON invoice_number_counters FOR ALL
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- ── Invoice candidate (pre-issue rated/taxed) ─────────────────────────────
--
-- Never mutated in place except the two forward transitions DRAFT->APPROVED
-- and APPROVED->ISSUED; a re-rating is always a brand new candidate row, not
-- an edit, so a stale candidate is always distinguishable from the one that
-- was actually approved/issued.
CREATE TABLE invoice_candidates (
    candidate_id                TEXT         PRIMARY KEY
        CHECK (candidate_id ~ '^cic_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id              UUID         NOT NULL,
    billing_account_id           TEXT         NOT NULL REFERENCES billing_accounts (billing_account_id),
    subscription_id               TEXT         NOT NULL,
    subscription_version_id       TEXT         NOT NULL,
    term_no                        INT          NOT NULL CHECK (term_no >= 1),
    currency_code                  CHAR(3)      NOT NULL,
    status                          VARCHAR(16)  NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT', 'APPROVED', 'ISSUED')),
    subtotal_amount                 NUMERIC      NOT NULL CHECK (subtotal_amount >= 0),
    tax_jurisdiction_code            VARCHAR(64)  NOT NULL CHECK (btrim(tax_jurisdiction_code) <> ''),
    tax_rate_basis_points            INT          NOT NULL CHECK (tax_rate_basis_points >= 0),
    tax_amount                       NUMERIC      NOT NULL CHECK (tax_amount >= 0),
    total_amount                     NUMERIC      NOT NULL CHECK (total_amount >= 0),
    created_at                       TIMESTAMPTZ  NOT NULL,
    created_by_principal_id          VARCHAR(255) NOT NULL,
    approved_at                      TIMESTAMPTZ,
    approved_by_principal_id         VARCHAR(255),
    issued_invoice_id                TEXT,
    CONSTRAINT invoice_candidates_total_correct CHECK (total_amount = subtotal_amount + tax_amount),
    CONSTRAINT invoice_candidates_approved_all_or_nothing CHECK (
        (approved_at IS NULL) = (approved_by_principal_id IS NULL)),
    CONSTRAINT invoice_candidates_approver_independent CHECK (
        approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id),
    CONSTRAINT invoice_candidates_issued_needs_approval CHECK (
        issued_invoice_id IS NULL OR approved_at IS NOT NULL)
);

-- One in-flight (not yet issued) candidate per subscription term: a second
-- GenerateInvoiceCandidate call for the same term while one is still
-- DRAFT/APPROVED does not create a competing candidate.
CREATE UNIQUE INDEX idx_invoice_candidates_one_in_flight ON invoice_candidates (subscription_id, term_no)
    WHERE status IN ('DRAFT', 'APPROVED');
CREATE INDEX idx_invoice_candidates_org ON invoice_candidates (organization_id);

CREATE FUNCTION enforce_invoice_candidate_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    approve_cols TEXT[] := ARRAY['status', 'approved_at', 'approved_by_principal_id'];
    issue_cols   TEXT[] := ARRAY['status', 'issued_invoice_id'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'invoice candidate % cannot be deleted' , OLD.candidate_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status = 'ISSUED' THEN
        RAISE EXCEPTION 'invoice candidate % is issued and immutable', OLD.candidate_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status = 'DRAFT' AND NEW.status = 'APPROVED'
       AND (to_jsonb(NEW) - approve_cols) IS NOT DISTINCT FROM (to_jsonb(OLD) - approve_cols) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'APPROVED' AND NEW.status = 'ISSUED'
       AND (to_jsonb(NEW) - issue_cols) IS NOT DISTINCT FROM (to_jsonb(OLD) - issue_cols) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'invoice candidate % only moves DRAFT -> APPROVED -> ISSUED, one field group at a time', OLD.candidate_id
        USING ERRCODE = 'CP001';
END;
$$;

CREATE TRIGGER trg_invoice_candidates_lifecycle
    BEFORE UPDATE OR DELETE ON invoice_candidates
    FOR EACH ROW EXECUTE FUNCTION enforce_invoice_candidate_lifecycle();

ALTER TABLE invoice_candidates ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_candidates FORCE ROW LEVEL SECURITY;
CREATE POLICY invoice_candidates_read ON invoice_candidates FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY invoice_candidates_seller_insert ON invoice_candidates FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY invoice_candidates_seller_update ON invoice_candidates FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- Candidate lines: the rated, line-level evidence a total was reached from
-- (COM-CTRL-019 lineage). statement_total_quantity_at_generate freezes what
-- the usage basis said at generate time, so IssueInvoice can detect drift
-- (negative path #42) without a second certification run.
CREATE TABLE invoice_candidate_lines (
    candidate_id                          TEXT        NOT NULL REFERENCES invoice_candidates (candidate_id),
    line_no                                 INT         NOT NULL CHECK (line_no >= 1),
    kind                                     VARCHAR(16) NOT NULL CHECK (kind IN ('RECURRING', 'USAGE')),
    description                             TEXT        NOT NULL,
    price_version_id                        TEXT        NOT NULL,
    component_key                           VARCHAR(64) NOT NULL,
    meter_key                               VARCHAR(128),
    statement_id                            TEXT,
    statement_total_quantity_at_generate     NUMERIC,
    quantity                                 NUMERIC,
    unit_amount                              NUMERIC,
    amount                                   NUMERIC     NOT NULL,
    PRIMARY KEY (candidate_id, line_no),
    CONSTRAINT invoice_candidate_lines_usage_has_statement CHECK (
        (kind = 'USAGE') = (statement_id IS NOT NULL AND meter_key IS NOT NULL AND statement_total_quantity_at_generate IS NOT NULL))
);

CREATE TRIGGER trg_invoice_candidate_lines_immutable
    BEFORE UPDATE OR DELETE ON invoice_candidate_lines
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE invoice_candidate_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_candidate_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY invoice_candidate_lines_read ON invoice_candidate_lines FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR EXISTS (SELECT 1 FROM invoice_candidates c WHERE c.candidate_id = invoice_candidate_lines.candidate_id
                       AND c.organization_id::text = NULLIF(current_setting('app.tenant_id', true), '')));
CREATE POLICY invoice_candidate_lines_seller_insert ON invoice_candidate_lines FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
-- An UPDATE/DELETE policy is granted (seller plane only) so the immutability
-- trigger is the actual enforcement point a seller-plane attempt reaches,
-- not merely RLS silently matching zero rows (same idiom as
-- price_versions_seller_update in migration 000006).
CREATE POLICY invoice_candidate_lines_seller_update ON invoice_candidate_lines FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY invoice_candidate_lines_seller_delete ON invoice_candidate_lines FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');

-- ── Platform commercial invoice (immutable once issued) ───────────────────
--
-- A frozen copy of the candidate's totals and lines at the instant it was
-- issued (negative path #45, #47: historical invoices render from bound
-- historical data, never from today's catalog/plan labels).
CREATE TABLE platform_commercial_invoices (
    invoice_id                  TEXT         PRIMARY KEY
        CHECK (invoice_id ~ '^cinv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    invoice_number                VARCHAR(64)  NOT NULL UNIQUE,
    organization_id                UUID         NOT NULL,
    billing_account_id             TEXT         NOT NULL REFERENCES billing_accounts (billing_account_id),
    candidate_id                    TEXT         NOT NULL UNIQUE REFERENCES invoice_candidates (candidate_id),
    subscription_id                  TEXT         NOT NULL,
    term_no                           INT          NOT NULL CHECK (term_no >= 1),
    currency_code                     CHAR(3)      NOT NULL,
    subtotal_amount                    NUMERIC      NOT NULL CHECK (subtotal_amount >= 0),
    tax_jurisdiction_code               VARCHAR(64)  NOT NULL,
    tax_rate_basis_points                INT          NOT NULL CHECK (tax_rate_basis_points >= 0),
    tax_amount                           NUMERIC      NOT NULL CHECK (tax_amount >= 0),
    total_amount                         NUMERIC      NOT NULL CHECK (total_amount >= 0),
    issued_at                             TIMESTAMPTZ  NOT NULL,
    issued_by_principal_id                VARCHAR(255) NOT NULL,
    CONSTRAINT platform_commercial_invoices_total_correct CHECK (total_amount = subtotal_amount + tax_amount)
);

CREATE INDEX idx_platform_commercial_invoices_org ON platform_commercial_invoices (organization_id);
CREATE INDEX idx_platform_commercial_invoices_subscription ON platform_commercial_invoices (subscription_id, term_no);

CREATE TRIGGER trg_platform_commercial_invoices_immutable
    BEFORE UPDATE OR DELETE ON platform_commercial_invoices
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE platform_commercial_invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform_commercial_invoices FORCE ROW LEVEL SECURITY;
CREATE POLICY platform_invoices_read ON platform_commercial_invoices FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY platform_invoices_seller_insert ON platform_commercial_invoices FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
-- UPDATE/DELETE granted (seller plane only) so a seller-plane attempt
-- reaches the immutability trigger rather than being silently filtered to
-- zero rows by RLS alone.
CREATE POLICY platform_invoices_seller_update ON platform_commercial_invoices FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY platform_invoices_seller_delete ON platform_commercial_invoices FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');

ALTER TABLE invoice_candidates ADD CONSTRAINT invoice_candidates_issued_invoice_fk
    FOREIGN KEY (issued_invoice_id) REFERENCES platform_commercial_invoices (invoice_id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE invoice_lines (
    invoice_id            TEXT        NOT NULL REFERENCES platform_commercial_invoices (invoice_id),
    line_no                INT         NOT NULL CHECK (line_no >= 1),
    kind                    VARCHAR(16) NOT NULL CHECK (kind IN ('RECURRING', 'USAGE')),
    description             TEXT        NOT NULL,
    price_version_id        TEXT        NOT NULL,
    component_key           VARCHAR(64) NOT NULL,
    meter_key               VARCHAR(128),
    statement_id            TEXT,
    quantity                 NUMERIC,
    unit_amount               NUMERIC,
    amount                    NUMERIC     NOT NULL,
    PRIMARY KEY (invoice_id, line_no)
);

CREATE TRIGGER trg_invoice_lines_immutable
    BEFORE UPDATE OR DELETE ON invoice_lines
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE invoice_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY invoice_lines_read ON invoice_lines FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR EXISTS (SELECT 1 FROM platform_commercial_invoices i WHERE i.invoice_id = invoice_lines.invoice_id
                       AND i.organization_id::text = NULLIF(current_setting('app.tenant_id', true), '')));
CREATE POLICY invoice_lines_seller_insert ON invoice_lines FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY invoice_lines_seller_update ON invoice_lines FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY invoice_lines_seller_delete ON invoice_lines FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');
