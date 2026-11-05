-- 000031_payment_terms.up.sql
--
-- REF-10 Business Calendar & Payment Terms (ZS-SVC-I-001 s4.20), the payment-term
-- half: "versioned payment-term calculation rules", "basis-date rule; due-day
-- methodology; discount/grace rules", "Historical due dates are never silently
-- recalculated after calendar/term update; calculation returns applied
-- version/provenance".
--
-- A payment term (NET30, EOM45) has VERSIONS. A version is immutable content (how
-- a due date is worked out) with a lifecycle Draft -> Active -> Superseded/Retired.
-- What changes after drafting is only its status and the end of its period, and
-- only along the legal edges. An approved version governs the period
-- [effective_from, ends_on); the database refuses two approved versions of one
-- term governing the same date, so replacing a term in force is always stated
-- (SupersedePaymentTerm names the version it replaces).

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE IF NOT EXISTS payment_terms (
    term_id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    term_code       VARCHAR(32)  NOT NULL,
    term_name       TEXT         NOT NULL,
    -- NULL = not specific to one jurisdiction.
    jurisdiction_id UUID         REFERENCES jurisdictions (jurisdiction_id),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by      TEXT         NOT NULL,
    CONSTRAINT uq_payment_term_code UNIQUE (term_code),
    CONSTRAINT ck_payment_term_code CHECK (term_code ~ '^[A-Z][A-Z0-9_]{1,31}$'),
    CONSTRAINT ck_payment_term_name CHECK (btrim(term_name) <> '')
);

CREATE TABLE IF NOT EXISTS payment_term_versions (
    term_version_id         UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    term_id                 UUID         NOT NULL REFERENCES payment_terms (term_id),
    version                 INTEGER      NOT NULL,
    status                  VARCHAR(12)  NOT NULL DEFAULT 'DRAFT',

    -- ── the rule (immutable) ────────────────────────────────────────────────
    -- Which date the term counts from; the caller supplies the dates.
    basis_date_rule         VARCHAR(16)  NOT NULL,
    -- NET_DAYS: basis + due_days. END_OF_MONTH_PLUS: end of the basis month +
    -- due_days. DAY_OF_NEXT_MONTH: the due_day_of_month of the month after the
    -- basis month (clamped to its length).
    due_method              VARCHAR(24)  NOT NULL,
    due_days                INTEGER      NOT NULL DEFAULT 0,
    due_day_of_month        SMALLINT,
    -- What to do when the due date is not a business day, and by whose calendar.
    business_day_convention VARCHAR(24)  NOT NULL DEFAULT 'NONE',
    -- Not a foreign key: calendars are never deleted, the service checks the calendar
    -- exists when a version is drafted and has a published version when it is
    -- approved, and an earlier migration (000010) must stay revertible on its own.
    calendar_code           VARCHAR(64),
    grace_days              INTEGER      NOT NULL DEFAULT 0,
    -- Early-payment discount: percent off if paid within discount_days of the basis.
    discount_percent        NUMERIC(7,4),
    discount_days           INTEGER,
    -- The term changes controlled credit policy: it is beyond the standard
    -- credit period and says so.
    controlled_credit       BOOLEAN      NOT NULL DEFAULT FALSE,
    rationale               TEXT         NOT NULL,
    source_ref              TEXT,
    -- The formula in words, and the digest of the rule, both derived by the service.
    formula_text            TEXT         NOT NULL,
    content_digest          CHAR(64)     NOT NULL,
    effective_from          DATE         NOT NULL,
    -- Set once, when the version stops governing (superseded or retired). The date it
    -- stopped is not the same as when that was recorded: superseded_at and retired_at
    -- say when, so "as known at an earlier time" can still see it open.
    ends_on            DATE,

    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by              TEXT         NOT NULL,
    approved_at             TIMESTAMPTZ,
    approved_by             TEXT,
    superseded_at           TIMESTAMPTZ,
    superseded_by_version_id UUID        REFERENCES payment_term_versions (term_version_id),
    retired_at              TIMESTAMPTZ,
    retired_by              TEXT,
    retire_reason           TEXT,

    -- The period an approved version governs.
    term_period             DATERANGE GENERATED ALWAYS AS (daterange(effective_from, ends_on, '[)')) STORED,

    CONSTRAINT uq_payment_term_version UNIQUE (term_id, version),
    CONSTRAINT ck_ptv_status CHECK (status IN ('DRAFT', 'ACTIVE', 'SUPERSEDED', 'RETIRED')),
    CONSTRAINT ck_ptv_version CHECK (version >= 1),
    CONSTRAINT ck_ptv_basis CHECK (basis_date_rule IN ('INVOICE_DATE', 'RECEIPT_DATE', 'DELIVERY_DATE')),
    CONSTRAINT ck_ptv_method CHECK (due_method IN ('NET_DAYS', 'END_OF_MONTH_PLUS', 'DAY_OF_NEXT_MONTH')),
    CONSTRAINT ck_ptv_due_days CHECK (due_days BETWEEN 0 AND 365),
    CONSTRAINT ck_ptv_day_of_month CHECK (
        (due_method = 'DAY_OF_NEXT_MONTH' AND due_day_of_month IS NOT NULL AND due_day_of_month BETWEEN 1 AND 31 AND due_days = 0)
        OR (due_method <> 'DAY_OF_NEXT_MONTH' AND due_day_of_month IS NULL)),
    CONSTRAINT ck_ptv_convention CHECK (business_day_convention IN
        ('NONE', 'FOLLOWING', 'PRECEDING', 'MODIFIED_FOLLOWING', 'MODIFIED_PRECEDING')),
    -- A convention needs a calendar to apply it by, and a calendar needs a convention.
    CONSTRAINT ck_ptv_calendar CHECK ((business_day_convention = 'NONE') = (calendar_code IS NULL)),
    CONSTRAINT ck_ptv_grace CHECK (grace_days BETWEEN 0 AND 90),
    CONSTRAINT ck_ptv_discount CHECK (
        (discount_percent IS NULL AND discount_days IS NULL)
        OR (discount_percent IS NOT NULL AND discount_days IS NOT NULL
            AND discount_percent > 0 AND discount_percent <= 100 AND discount_days BETWEEN 1 AND 365)),
    -- Beyond the standard credit period a term must say it changes controlled credit.
    CONSTRAINT ck_ptv_credit CHECK (due_days <= 90 OR controlled_credit),
    CONSTRAINT ck_ptv_text CHECK (btrim(rationale) <> '' AND btrim(formula_text) <> ''),
    CONSTRAINT ck_ptv_digest CHECK (content_digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT ck_ptv_period CHECK (ends_on IS NULL OR ends_on > effective_from),
    -- Approval is by someone other than the author.
    CONSTRAINT ck_ptv_approver_independent CHECK (approved_by IS NULL OR approved_by <> created_by),
    CONSTRAINT ck_ptv_state_shape CHECK (
        (status = 'DRAFT'      AND approved_at IS NULL AND approved_by IS NULL AND superseded_at IS NULL AND superseded_by_version_id IS NULL
                               AND retired_at IS NULL AND retired_by IS NULL AND retire_reason IS NULL)
     OR (status = 'ACTIVE'     AND approved_at IS NOT NULL AND approved_by IS NOT NULL AND superseded_at IS NULL
                               AND superseded_by_version_id IS NULL AND retired_at IS NULL)
     OR (status = 'SUPERSEDED' AND approved_at IS NOT NULL AND approved_by IS NOT NULL AND superseded_at IS NOT NULL
                               AND superseded_by_version_id IS NOT NULL AND ends_on IS NOT NULL AND retired_at IS NULL)
     OR (status = 'RETIRED'    AND retired_at IS NOT NULL AND retired_by IS NOT NULL AND btrim(COALESCE(retire_reason, '')) <> ''
                               AND superseded_at IS NULL
                               AND ((approved_at IS NULL AND approved_by IS NULL) OR (approved_at IS NOT NULL AND approved_by IS NOT NULL AND ends_on IS NOT NULL)))),
    -- An approved version governs a date at most once: replacing a term in force is
    -- stated, not a side effect.
    CONSTRAINT excl_ptv_one_in_force EXCLUDE USING GIST (term_id WITH =, term_period WITH &&) WHERE (approved_at IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_ptv_resolve ON payment_term_versions (term_id, effective_from) WHERE approved_at IS NOT NULL;

-- A version's rule is immutable. After drafting only the lifecycle moves, along
-- DRAFT -> ACTIVE, DRAFT -> RETIRED, ACTIVE -> SUPERSEDED and ACTIVE -> RETIRED,
-- and ends_on is set once, on the way out.
CREATE OR REPLACE FUNCTION payment_term_version_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'payment_term_versions is append-only (% refused)', TG_OP USING ERRCODE = '23514';
    END IF;
    IF NEW.term_version_id <> OLD.term_version_id OR NEW.term_id <> OLD.term_id OR NEW.version <> OLD.version
       OR NEW.basis_date_rule <> OLD.basis_date_rule OR NEW.due_method <> OLD.due_method OR NEW.due_days <> OLD.due_days
       OR NEW.due_day_of_month IS DISTINCT FROM OLD.due_day_of_month
       OR NEW.business_day_convention <> OLD.business_day_convention OR NEW.calendar_code IS DISTINCT FROM OLD.calendar_code
       OR NEW.grace_days <> OLD.grace_days OR NEW.discount_percent IS DISTINCT FROM OLD.discount_percent
       OR NEW.discount_days IS DISTINCT FROM OLD.discount_days OR NEW.controlled_credit <> OLD.controlled_credit
       OR NEW.rationale <> OLD.rationale OR NEW.source_ref IS DISTINCT FROM OLD.source_ref
       OR NEW.formula_text <> OLD.formula_text OR NEW.content_digest <> OLD.content_digest
       OR NEW.effective_from <> OLD.effective_from OR NEW.created_at <> OLD.created_at OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'a payment term version''s rule is immutable: draft a new version' USING ERRCODE = '23514';
    END IF;
    IF NEW.status <> OLD.status AND NOT (
           (OLD.status = 'DRAFT'  AND NEW.status IN ('ACTIVE', 'RETIRED'))
        OR (OLD.status = 'ACTIVE' AND NEW.status IN ('SUPERSEDED', 'RETIRED'))) THEN
        RAISE EXCEPTION 'a payment term version cannot move from % to %', OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;
    IF NEW.status = OLD.status AND OLD.status IN ('SUPERSEDED', 'RETIRED') AND ROW(NEW.*) IS DISTINCT FROM ROW(OLD.*) THEN
        RAISE EXCEPTION 'a % payment term version is final', OLD.status USING ERRCODE = '23514';
    END IF;
    IF NEW.ends_on IS DISTINCT FROM OLD.ends_on AND NOT (
           OLD.status = 'ACTIVE' AND NEW.status IN ('SUPERSEDED', 'RETIRED') AND OLD.ends_on IS NULL) THEN
        RAISE EXCEPTION 'the end of a payment term version''s period is set once, when it stops governing' USING ERRCODE = '23514';
    END IF;
    IF OLD.status <> 'DRAFT' AND (NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by IS DISTINCT FROM OLD.approved_by) THEN
        RAISE EXCEPTION 'the approval of a payment term version is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_payment_term_version_guard ON payment_term_versions;
CREATE TRIGGER trg_payment_term_version_guard
    BEFORE UPDATE OR DELETE ON payment_term_versions
    FOR EACH ROW EXECUTE FUNCTION payment_term_version_guard();
DROP TRIGGER IF EXISTS trg_payment_term_version_no_truncate ON payment_term_versions;
CREATE TRIGGER trg_payment_term_version_no_truncate
    BEFORE TRUNCATE ON payment_term_versions
    FOR EACH STATEMENT EXECUTE FUNCTION payment_term_version_guard();

-- A term's identity is immutable and it is never deleted.
CREATE OR REPLACE FUNCTION payment_term_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'payment_terms is append-only (% refused)', TG_OP USING ERRCODE = '23514';
    END IF;
    RAISE EXCEPTION 'a payment term''s identity is immutable' USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_payment_term_guard ON payment_terms;
CREATE TRIGGER trg_payment_term_guard
    BEFORE UPDATE OR DELETE ON payment_terms
    FOR EACH ROW EXECUTE FUNCTION payment_term_guard();
DROP TRIGGER IF EXISTS trg_payment_term_no_truncate ON payment_terms;
CREATE TRIGGER trg_payment_term_no_truncate
    BEFORE TRUNCATE ON payment_terms
    FOR EACH STATEMENT EXECUTE FUNCTION payment_term_guard();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE ON payment_terms, payment_term_versions TO app_jurisdiction_rules;
    END IF;
END
$$;
