-- 000018_add_com05_evidence_dispute.up.sql
-- COM-05 gap-remediation, following a skeptical re-audit: §7 explicitly
-- names CommercialEvidencePackage as a canonical entity and nothing
-- implemented it (D1); §6 names a "Dispute/chargeback" dimension
-- (None/Open/Won/Lost/Resolved) independent of invoice issuance, and
-- nothing implemented that either (D4, COM-CTRL-029, negative path #32).
--
-- Custom SQLSTATE reused: CP001 immutable/lifecycle violation.

-- ── D1: CommercialEvidencePackage ──────────────────────────────────────────
--
-- An immutable, sealed manifest tying one issued invoice to its exact
-- source lineage (subscription version, price version content hashes,
-- usage statement totals, tax evidence) in one addressable artifact —
-- proof without re-querying multiple live tables. Sealed automatically at
-- issue time, in the same transaction as the invoice itself; never a
-- separate operator command.
CREATE TABLE commercial_evidence_packages (
    package_id                TEXT         PRIMARY KEY
        CHECK (package_id ~ '^cevp_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    invoice_id                  TEXT         NOT NULL UNIQUE REFERENCES platform_commercial_invoices (invoice_id),
    organization_id              UUID         NOT NULL,
    manifest                       JSONB        NOT NULL,
    manifest_sha256                 CHAR(64)     NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    sealed_at                        TIMESTAMPTZ  NOT NULL,
    sealed_by_principal_id            VARCHAR(255) NOT NULL
);

CREATE INDEX idx_commercial_evidence_packages_org ON commercial_evidence_packages (organization_id);

CREATE TRIGGER trg_commercial_evidence_packages_immutable
    BEFORE UPDATE OR DELETE ON commercial_evidence_packages
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE commercial_evidence_packages ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_evidence_packages FORCE ROW LEVEL SECURITY;
CREATE POLICY commercial_evidence_packages_read ON commercial_evidence_packages FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY commercial_evidence_packages_insert ON commercial_evidence_packages FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
-- UPDATE/DELETE granted under seller plane too, so an attempt reaches the
-- immutability trigger rather than being silently filtered to zero rows by
-- RLS alone (the exact bug class found and fixed twice already this
-- session for other COM-05/COM-03 evidence tables).
CREATE POLICY commercial_evidence_packages_update ON commercial_evidence_packages FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY commercial_evidence_packages_delete ON commercial_evidence_packages FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');

-- ── D4: Dispute / chargeback ───────────────────────────────────────────────
--
-- Purely orthogonal tracking, independent of invoice issuance and payment
-- collection: nothing here, or in any of its commands, writes to
-- payment_attempts or platform_commercial_invoices — "preserve paid
-- collection history and separate dispute state" (doc, verbatim). Forward-
-- only lifecycle: OPEN -> (WON | LOST) -> RESOLVED.
CREATE TABLE dispute_cases (
    dispute_id                 TEXT         PRIMARY KEY
        CHECK (dispute_id ~ '^cdis_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id              UUID         NOT NULL,
    invoice_id                    TEXT         NOT NULL REFERENCES platform_commercial_invoices (invoice_id),
    payment_attempt_id              TEXT         NOT NULL REFERENCES payment_attempts (attempt_id),
    status                            VARCHAR(16)  NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'WON', 'LOST', 'RESOLVED')),
    reason                              TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    opened_at                            TIMESTAMPTZ  NOT NULL,
    opened_by_principal_id                VARCHAR(255) NOT NULL,
    outcome_recorded_at                    TIMESTAMPTZ,
    outcome_recorded_by_principal_id        VARCHAR(255),
    resolved_at                              TIMESTAMPTZ,
    resolved_by_principal_id                  VARCHAR(255),
    resolution_notes                            TEXT,
    -- An optional evidentiary link to a refund a LOST dispute's money
    -- reversal went through — never automated, never enforced here; the
    -- refund itself is created through the existing RequestRefund command.
    related_refund_id                             TEXT REFERENCES refund_requests (refund_id),
    CONSTRAINT dispute_cases_outcome_all_or_nothing CHECK (
        (outcome_recorded_at IS NULL) = (status = 'OPEN')
        AND (outcome_recorded_at IS NULL) = (outcome_recorded_by_principal_id IS NULL)),
    CONSTRAINT dispute_cases_resolved_all_or_nothing CHECK (
        (resolved_at IS NULL) = (status <> 'RESOLVED')
        AND (resolved_at IS NULL) = (resolved_by_principal_id IS NULL))
);

CREATE INDEX idx_dispute_cases_invoice ON dispute_cases (invoice_id);
CREATE INDEX idx_dispute_cases_attempt ON dispute_cases (payment_attempt_id);
CREATE INDEX idx_dispute_cases_org ON dispute_cases (organization_id);

CREATE FUNCTION enforce_dispute_case_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dispute case % cannot be deleted', OLD.dispute_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status = 'RESOLVED' THEN
        RAISE EXCEPTION 'dispute case % is resolved and immutable', OLD.dispute_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status = 'OPEN' AND NEW.status NOT IN ('WON', 'LOST') THEN
        RAISE EXCEPTION 'dispute case % may only move from OPEN to WON or LOST', OLD.dispute_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status IN ('WON', 'LOST') AND NEW.status <> 'RESOLVED' THEN
        RAISE EXCEPTION 'dispute case % may only move from % to RESOLVED', OLD.dispute_id, OLD.status USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_dispute_cases_lifecycle
    BEFORE UPDATE OR DELETE ON dispute_cases
    FOR EACH ROW EXECUTE FUNCTION enforce_dispute_case_lifecycle();

ALTER TABLE dispute_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE dispute_cases FORCE ROW LEVEL SECURITY;
CREATE POLICY dispute_cases_read ON dispute_cases FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY dispute_cases_seller_insert ON dispute_cases FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY dispute_cases_seller_update ON dispute_cases FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY dispute_cases_seller_delete ON dispute_cases FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');
