-- 000021_accounting_kernel_close_fx.up.sql
--
-- Global Accounting Kernel, Wave 3 (period close / FX): the exchange-rate store (§23.1), revaluation runs (§24), signed balance
-- snapshots (§10.2, §26) and period seals that stop a posting slipping into a period after its close watermark (§36).

-- ── the ledger keeps the transaction-currency basis of each entry, so open foreign items can be valued at period end ────

ALTER TABLE ledger_entries ADD COLUMN transaction_amount NUMERIC(28,8), ADD COLUMN transaction_currency CHAR(3);
CREATE INDEX idx_ledger_entries_fx ON ledger_entries (tenant_id, legal_entity_id, book_id, account_code, transaction_currency)
    WHERE transaction_currency IS NOT NULL;

-- an account is monetary (revalued at period end) when flagged, or when it is a control type that holds money owed or held
ALTER TABLE chart_of_accounts ADD COLUMN monetary BOOLEAN NOT NULL DEFAULT FALSE;

-- ── exchange rates (§23.1): immutable, sourced, typed ──────────────────────────────────────────────────────────────────

CREATE TABLE fx_rates (
    fx_rate_id         UUID PRIMARY KEY,
    tenant_id          UUID          NOT NULL,
    base_currency      CHAR(3)       NOT NULL,
    quote_currency     CHAR(3)       NOT NULL,
    rate               NUMERIC(28,12) NOT NULL,
    rate_type          VARCHAR(24)   NOT NULL,
    effective_at       TIMESTAMPTZ   NOT NULL,
    rate_date          DATE          NOT NULL,
    source             VARCHAR(120)  NOT NULL,
    source_rate_id     VARCHAR(200)  NOT NULL,
    ingested_at        TIMESTAMPTZ   NOT NULL DEFAULT now(),
    ingested_by        VARCHAR(255)  NOT NULL,
    override           BOOLEAN       NOT NULL DEFAULT FALSE,
    override_reason    TEXT,
    override_approved_by VARCHAR(255),
    CONSTRAINT fx_rates_positive CHECK (rate > 0),
    CONSTRAINT fx_rates_pair CHECK (base_currency <> quote_currency),
    CONSTRAINT fx_rates_type CHECK (rate_type IN ('SPOT', 'TRANSACTION', 'CLOSING', 'AVERAGE', 'HISTORICAL', 'CONTRACTUAL', 'APPROVED_EXTENSION')),
    CONSTRAINT fx_rates_source UNIQUE (tenant_id, source, source_rate_id),
    CONSTRAINT fx_rates_override CHECK (NOT override OR (COALESCE(btrim(override_reason), '') <> '' AND override_approved_by IS NOT NULL AND override_approved_by <> ingested_by))
);
CREATE INDEX idx_fx_rates_lookup ON fx_rates (tenant_id, base_currency, quote_currency, rate_type, effective_at DESC);
ALTER TABLE fx_rates ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_rates FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_rates
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE TRIGGER fx_rates_no_change BEFORE UPDATE OR DELETE ON fx_rates FOR EACH ROW EXECUTE FUNCTION kernel_append_only();

-- A rate keyed in by hand is an override: it is requested with a reason and becomes a rate only when someone else approves it.
CREATE TABLE fx_rate_override_requests (
    request_id       UUID PRIMARY KEY,
    tenant_id        UUID           NOT NULL,
    base_currency    CHAR(3)        NOT NULL,
    quote_currency   CHAR(3)        NOT NULL,
    rate             NUMERIC(28,12) NOT NULL,
    rate_type        VARCHAR(24)    NOT NULL,
    effective_at     TIMESTAMPTZ    NOT NULL,
    rate_date        DATE           NOT NULL,
    source           VARCHAR(120)   NOT NULL,
    source_rate_id   VARCHAR(200)   NOT NULL,
    reason           TEXT           NOT NULL,
    requested_by     VARCHAR(255)   NOT NULL,
    requested_at     TIMESTAMPTZ    NOT NULL DEFAULT now(),
    status           VARCHAR(10)    NOT NULL DEFAULT 'PENDING',
    decided_by       VARCHAR(255),
    decided_at       TIMESTAMPTZ,
    decision_note    TEXT,
    fx_rate_id       UUID REFERENCES fx_rates(fx_rate_id),
    CONSTRAINT fx_override_status CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED')),
    CONSTRAINT fx_override_reason CHECK (btrim(reason) <> ''),
    CONSTRAINT fx_override_positive CHECK (rate > 0)
);
ALTER TABLE fx_rate_override_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE fx_rate_override_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON fx_rate_override_requests
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE OR REPLACE FUNCTION fx_override_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a rate override request is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'PENDING' THEN
        RAISE EXCEPTION 'a decided rate override request is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['status', 'decided_by', 'decided_at', 'decision_note', 'fx_rate_id']) IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'decided_by', 'decided_at', 'decision_note', 'fx_rate_id']) THEN
        RAISE EXCEPTION 'the rate being overridden cannot change after it is requested' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'APPROVED' AND (NEW.decided_by IS NULL OR NEW.decided_by = OLD.requested_by OR NEW.fx_rate_id IS NULL) THEN
        RAISE EXCEPTION 'a rate override is approved by someone other than the person who asked for it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'REJECTED' AND (NEW.decided_by IS NULL OR COALESCE(btrim(NEW.decision_note), '') = '') THEN
        RAISE EXCEPTION 'a rejected rate override needs a reason' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER fx_override_guard_trg BEFORE UPDATE OR DELETE ON fx_rate_override_requests FOR EACH ROW EXECUTE FUNCTION fx_override_guard();

-- ── revaluation runs (§24): one per entity, book, period and run type; the population they valued is kept ──────────────

CREATE TABLE revaluation_runs (
    revaluation_run_id   UUID PRIMARY KEY,
    tenant_id            UUID          NOT NULL,
    legal_entity_id      UUID          NOT NULL,
    accounting_book_id   UUID          NOT NULL REFERENCES accounting_books(book_id),
    fiscal_period        VARCHAR(20)   NOT NULL,
    run_type             VARCHAR(16)   NOT NULL DEFAULT 'UNREALIZED',
    valuation_date       DATE          NOT NULL,
    policy_id            VARCHAR(40)   NOT NULL,
    ledger_watermark     BIGINT        NOT NULL,
    population           JSONB         NOT NULL,
    item_count           INTEGER       NOT NULL,
    total_gain           NUMERIC(24,6) NOT NULL,
    total_loss           NUMERIC(24,6) NOT NULL,
    journal_id           UUID REFERENCES journal_headers(journal_id),
    reversal_journal_id  UUID REFERENCES journal_headers(journal_id),
    created_by           VARCHAR(255)  NOT NULL,
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT revaluation_runs_scope UNIQUE (tenant_id, legal_entity_id, accounting_book_id, fiscal_period, run_type),
    CONSTRAINT revaluation_runs_type CHECK (run_type IN ('UNREALIZED'))
);
ALTER TABLE revaluation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE revaluation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON revaluation_runs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE TRIGGER revaluation_runs_no_change BEFORE UPDATE OR DELETE ON revaluation_runs FOR EACH ROW EXECUTE FUNCTION kernel_append_only();

-- ── signed balance snapshots (§10.2, §26): the balances at a ledger watermark, retained and signed ──────────────────────

CREATE TABLE balance_snapshots (
    snapshot_id          UUID PRIMARY KEY,
    tenant_id            UUID          NOT NULL,
    legal_entity_id      UUID          NOT NULL,
    accounting_book_id   UUID          NOT NULL REFERENCES accounting_books(book_id),
    fiscal_period        VARCHAR(20)   NOT NULL,
    as_of                DATE          NOT NULL,
    purpose              VARCHAR(24)   NOT NULL,
    ledger_watermark     BIGINT        NOT NULL,
    lines                JSONB         NOT NULL,
    account_count        INTEGER       NOT NULL,
    total_debit          NUMERIC(24,2) NOT NULL,
    total_credit         NUMERIC(24,2) NOT NULL,
    content_hash         CHAR(64)      NOT NULL,
    signature            CHAR(64)      NOT NULL,
    key_id               VARCHAR(80)   NOT NULL,
    signed_by            VARCHAR(255)  NOT NULL,
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT balance_snapshots_purpose CHECK (purpose IN ('CLOSE', 'RECLOSE', 'CONSOLIDATION_FREEZE', 'AD_HOC')),
    CONSTRAINT balance_snapshots_balanced CHECK (total_debit = total_credit)
);
CREATE INDEX idx_balance_snapshots_scope ON balance_snapshots (tenant_id, legal_entity_id, accounting_book_id, fiscal_period, created_at DESC);
ALTER TABLE balance_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE balance_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON balance_snapshots
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE TRIGGER balance_snapshots_no_change BEFORE UPDATE OR DELETE ON balance_snapshots FOR EACH ROW EXECUTE FUNCTION kernel_append_only();

-- ── period seals (§36): the close watermark that no later posting can slip under ───────────────────────────────────────

CREATE TABLE period_seals (
    tenant_id          UUID          NOT NULL,
    legal_entity_id    UUID          NOT NULL,
    fiscal_period      VARCHAR(20)   NOT NULL,
    status             VARCHAR(10)   NOT NULL,
    watermark          BIGINT        NOT NULL,
    sealed_by          VARCHAR(255)  NOT NULL,
    sealed_at          TIMESTAMPTZ   NOT NULL,
    reopened_by        VARCHAR(255),
    reopened_at        TIMESTAMPTZ,
    reopen_authority   TEXT,
    reopen_reason      TEXT,
    seal_count         INTEGER       NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant_id, legal_entity_id, fiscal_period),
    CONSTRAINT period_seals_status CHECK (status IN ('SEALED', 'REOPENED')),
    CONSTRAINT period_seals_reopen CHECK (status = 'SEALED' OR (COALESCE(btrim(reopen_authority), '') <> '' AND COALESCE(btrim(reopen_reason), '') <> '' AND reopened_by IS NOT NULL))
);
ALTER TABLE period_seals ENABLE ROW LEVEL SECURITY;
ALTER TABLE period_seals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON period_seals
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE OR REPLACE FUNCTION period_seal_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a period seal is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'SEALED' AND OLD.status = 'SEALED' THEN
        RAISE EXCEPTION 'the period is already sealed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'SEALED' AND NEW.watermark < OLD.watermark THEN
        RAISE EXCEPTION 'a re-seal cannot move the watermark back' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER period_seal_guard_trg BEFORE UPDATE OR DELETE ON period_seals FOR EACH ROW EXECUTE FUNCTION period_seal_guard();

CREATE TABLE period_seal_events (
    event_id               UUID PRIMARY KEY,
    tenant_id              UUID          NOT NULL,
    legal_entity_id        UUID          NOT NULL,
    fiscal_period          VARCHAR(20)   NOT NULL,
    kind                   VARCHAR(10)   NOT NULL,
    watermark              BIGINT        NOT NULL,
    actor                  VARCHAR(255)  NOT NULL,
    authority_ref          TEXT,
    reason                 TEXT,
    entries_since_reopen   BIGINT,
    at                     TIMESTAMPTZ   NOT NULL DEFAULT now(),
    CONSTRAINT period_seal_events_kind CHECK (kind IN ('SEAL', 'REOPEN', 'RESEAL'))
);
CREATE INDEX idx_period_seal_events_scope ON period_seal_events (tenant_id, legal_entity_id, fiscal_period, at);
ALTER TABLE period_seal_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE period_seal_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON period_seal_events
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
CREATE TRIGGER period_seal_events_no_change BEFORE UPDATE OR DELETE ON period_seal_events FOR EACH ROW EXECUTE FUNCTION kernel_append_only();
