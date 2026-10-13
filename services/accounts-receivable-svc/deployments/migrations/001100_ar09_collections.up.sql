-- Migration: 001100_ar09_collections.up.sql
-- AR-09 Collections & Dunning (spec ZS-SVC-C-001 section 12). Collections owns cases, dunning stage, communication history,
-- promises, case-level holds and escalation. It owns NO balance: no column here stores a residual as truth, and the only
-- amount is a customer's promised amount (a promise, not a receivable). Overdue is derived from AR-07, never a status.
--   ar09_dunning_policies  immutable, versioned per (entity, dunning_profile); stages + contact policy (data, not code)
--   ar09_collection_cases  one non-terminal case per customer/currency; aging snapshot captured at open (evidence)
--   ar09_case_items        open-item references only
--   ar09_dunning_actions   append-only communication / suppression evidence; one SENT/DELIVERED per (case, stage)
--   ar09_promises          append-only versions (supersedes chain); a changed promise is a NEW row (#28)
--   ar09_holds             case-level collection holds
--   ar09_history           append-only evidence of every case state change

CREATE TABLE IF NOT EXISTS ar09_dunning_policies (
    policy_id        UUID PRIMARY KEY,
    tenant_id        UUID         NOT NULL,
    legal_entity_id  UUID         NOT NULL,
    dunning_profile  VARCHAR(64)  NOT NULL CHECK (dunning_profile <> ''),
    version          INTEGER      NOT NULL CHECK (version >= 1),
    effective_from   DATE         NOT NULL,
    stages           JSONB        NOT NULL CHECK (jsonb_typeof(stages) = 'array' AND jsonb_array_length(stages) > 0),
    blackout_weekdays SMALLINT[]  NOT NULL DEFAULT '{}',
    blackout_dates   DATE[]       NOT NULL DEFAULT '{}',
    max_notices_per_window INTEGER NOT NULL DEFAULT 0 CHECK (max_notices_per_window >= 0),
    window_days      INTEGER      NOT NULL DEFAULT 0 CHECK (window_days >= 0),
    promise_grace_days INTEGER    NOT NULL DEFAULT 0 CHECK (promise_grace_days >= 0),
    published_by     VARCHAR(255) NOT NULL,
    published_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_ar09_policy_version UNIQUE (tenant_id, legal_entity_id, dunning_profile, version),
    CONSTRAINT ar09_policy_window CHECK ((max_notices_per_window = 0) = (window_days = 0))
);

CREATE TABLE IF NOT EXISTS ar09_collection_cases (
    case_id          UUID PRIMARY KEY,
    tenant_id        UUID         NOT NULL,
    legal_entity_id  UUID         NOT NULL,
    customer_party_id UUID        NOT NULL,
    currency         CHAR(3)      NOT NULL,
    state            VARCHAR(16)  NOT NULL DEFAULT 'ELIGIBLE'
        CONSTRAINT ar09_state_valid CHECK (state IN ('ELIGIBLE','ACTIVE','PROMISE_TO_PAY','HELD','DISPUTED','ESCALATED','RESOLVED','CLOSED')),
    state_before_hold VARCHAR(16),
    owner_principal  VARCHAR(255) NOT NULL,
    dunning_profile  VARCHAR(64)  NOT NULL,
    policy_id        UUID         NOT NULL REFERENCES ar09_dunning_policies(policy_id),
    dunning_stage    INTEGER      NOT NULL DEFAULT 0 CHECK (dunning_stage >= 0),
    aging_snapshot   JSONB        NOT NULL,
    referral_reason  VARCHAR(500),
    escalated_by     VARCHAR(255),
    escalated_at     TIMESTAMPTZ,
    resolution       JSONB,
    resolved_at      TIMESTAMPTZ,
    closed_by        VARCHAR(255),
    close_reason     VARCHAR(500),
    version          INTEGER      NOT NULL DEFAULT 1,
    created_by       VARCHAR(255) NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar09_one_live_case ON ar09_collection_cases (tenant_id, legal_entity_id, customer_party_id, currency)
    WHERE state NOT IN ('RESOLVED','CLOSED');
CREATE INDEX IF NOT EXISTS idx_ar09_case_customer ON ar09_collection_cases (tenant_id, legal_entity_id, customer_party_id);

CREATE TABLE IF NOT EXISTS ar09_case_items (
    tenant_id    UUID        NOT NULL,
    case_id      UUID        NOT NULL REFERENCES ar09_collection_cases(case_id),
    open_item_id UUID        NOT NULL,
    added_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (case_id, open_item_id)
);

CREATE TABLE IF NOT EXISTS ar09_dunning_actions (
    action_id        UUID PRIMARY KEY,
    seq              BIGSERIAL    NOT NULL,
    tenant_id        UUID         NOT NULL,
    legal_entity_id  UUID         NOT NULL,
    case_id          UUID         NOT NULL REFERENCES ar09_collection_cases(case_id),
    customer_party_id UUID        NOT NULL,
    stage_no         INTEGER      NOT NULL CHECK (stage_no >= 1),
    channel          VARCHAR(32)  NOT NULL,
    template_key     VARCHAR(128) NOT NULL,
    delivery_result  VARCHAR(16)  NOT NULL CHECK (delivery_result IN ('SENT','DELIVERED','DELIVERY_FAILED','SUPPRESSED')),
    suppression_reason VARCHAR(64),
    failure_detail   VARCHAR(500),
    provider_ref     VARCHAR(255),
    notice_artifact_hash CHAR(64),
    notice_body      TEXT,
    aging_snapshot   JSONB,
    actor_id         VARCHAR(255) NOT NULL,
    automated        BOOLEAN      NOT NULL DEFAULT FALSE,
    recorded_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ar09_action_suppression CHECK ((delivery_result = 'SUPPRESSED') = (suppression_reason IS NOT NULL)),
    CONSTRAINT ar09_action_artifact CHECK (delivery_result = 'SUPPRESSED' OR notice_artifact_hash IS NOT NULL)
);
-- exactly one delivered/sent notice per (case, stage): the idempotency guard for replays and concurrent schedulers.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar09_one_notice_per_stage ON ar09_dunning_actions (tenant_id, case_id, stage_no)
    WHERE delivery_result IN ('SENT','DELIVERED');
CREATE INDEX IF NOT EXISTS idx_ar09_actions_case ON ar09_dunning_actions (tenant_id, case_id, seq);
CREATE INDEX IF NOT EXISTS idx_ar09_actions_customer ON ar09_dunning_actions (tenant_id, legal_entity_id, customer_party_id, recorded_at);

CREATE TABLE IF NOT EXISTS ar09_promises (
    promise_id       UUID PRIMARY KEY,
    tenant_id        UUID          NOT NULL,
    legal_entity_id  UUID          NOT NULL,
    case_id          UUID          NOT NULL REFERENCES ar09_collection_cases(case_id),
    version_no       INTEGER       NOT NULL CHECK (version_no >= 1),
    promised_date    DATE          NOT NULL,
    promised_amount  NUMERIC(20,4) NOT NULL CONSTRAINT ar09_promise_amount_positive CHECK (promised_amount > 0),
    currency         CHAR(3)       NOT NULL,
    supersedes       UUID          REFERENCES ar09_promises(promise_id),
    reason_code      VARCHAR(64),
    recorded_by      VARCHAR(255)  NOT NULL,
    recorded_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_ar09_promise_version UNIQUE (case_id, version_no),
    CONSTRAINT ar09_promise_chain CHECK ((version_no = 1) = (supersedes IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar09_promise_superseded_once ON ar09_promises (supersedes) WHERE supersedes IS NOT NULL;

CREATE TABLE IF NOT EXISTS ar09_holds (
    hold_id          UUID PRIMARY KEY,
    tenant_id        UUID         NOT NULL,
    legal_entity_id  UUID         NOT NULL,
    case_id          UUID         NOT NULL REFERENCES ar09_collection_cases(case_id),
    reason_code      VARCHAR(64)  NOT NULL CHECK (reason_code <> ''),
    evidence_id      VARCHAR(255),
    placed_by        VARCHAR(255) NOT NULL,
    placed_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    released_by      VARCHAR(255),
    released_at      TIMESTAMPTZ,
    release_reason   VARCHAR(64),
    CONSTRAINT ar09_hold_release_consistent CHECK ((released_at IS NULL) = (released_by IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar09_one_active_hold ON ar09_holds (tenant_id, case_id) WHERE released_at IS NULL;

CREATE TABLE IF NOT EXISTS ar09_history (
    history_id       UUID PRIMARY KEY,
    seq              BIGSERIAL    NOT NULL,
    tenant_id        UUID         NOT NULL,
    legal_entity_id  UUID         NOT NULL,
    case_id          UUID         NOT NULL REFERENCES ar09_collection_cases(case_id),
    event            VARCHAR(40)  NOT NULL,
    from_state       VARCHAR(16),
    to_state         VARCHAR(16),
    version_after    INTEGER      NOT NULL,
    actor_id         VARCHAR(255) NOT NULL,
    reason_code      VARCHAR(64),
    detail           JSONB,
    at               TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ar09_hist_case ON ar09_history (tenant_id, case_id, seq);

-- Append-only evidence.
DROP TRIGGER IF EXISTS ar09_actions_append_only ON ar09_dunning_actions;
CREATE TRIGGER ar09_actions_append_only BEFORE UPDATE OR DELETE ON ar09_dunning_actions FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();
DROP TRIGGER IF EXISTS ar09_hist_append_only ON ar09_history;
CREATE TRIGGER ar09_hist_append_only BEFORE UPDATE OR DELETE ON ar09_history FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();
DROP TRIGGER IF EXISTS ar09_policy_immutable ON ar09_dunning_policies;
CREATE TRIGGER ar09_policy_immutable BEFORE UPDATE OR DELETE ON ar09_dunning_policies FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();
DROP TRIGGER IF EXISTS ar09_promises_append_only ON ar09_promises;
CREATE TRIGGER ar09_promises_append_only BEFORE UPDATE OR DELETE ON ar09_promises FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

-- A changed promise is a new version chained to the immediately previous one, with a reason (#28): a promise date or amount
-- can never be changed without history.
CREATE OR REPLACE FUNCTION ar09_guard_promise() RETURNS trigger AS $$
DECLARE prev ar09_promises%ROWTYPE;
BEGIN
    SELECT * INTO prev FROM ar09_promises WHERE tenant_id = NEW.tenant_id AND case_id = NEW.case_id ORDER BY version_no DESC LIMIT 1;
    IF NOT FOUND THEN
        IF NEW.version_no <> 1 THEN
            RAISE EXCEPTION 'AR09_PROMISE_HISTORY: the first promise of a case is version 1';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.version_no <> prev.version_no + 1 OR NEW.supersedes IS DISTINCT FROM prev.promise_id THEN
        RAISE EXCEPTION 'AR09_PROMISE_HISTORY: a changed promise must be a new version that supersedes the previous one';
    END IF;
    IF COALESCE(NEW.reason_code, '') = '' THEN
        RAISE EXCEPTION 'AR09_PROMISE_HISTORY: changing a promise requires a reason code';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar09_promise_guard ON ar09_promises;
CREATE TRIGGER ar09_promise_guard BEFORE INSERT ON ar09_promises FOR EACH ROW EXECUTE FUNCTION ar09_guard_promise();

-- A hold row never changes except to record its release, once.
CREATE OR REPLACE FUNCTION ar09_guard_hold() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR09_HOLD_NOT_DELETABLE: a collection hold is never deleted';
    END IF;
    IF OLD.released_at IS NOT NULL THEN
        RAISE EXCEPTION 'AR09_HOLD_RELEASED: a released hold cannot change';
    END IF;
    IF NEW.hold_id <> OLD.hold_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.case_id <> OLD.case_id OR NEW.reason_code <> OLD.reason_code
       OR NEW.placed_by <> OLD.placed_by OR NEW.placed_at <> OLD.placed_at OR NEW.evidence_id IS DISTINCT FROM OLD.evidence_id THEN
        RAISE EXCEPTION 'AR09_HOLD_IMMUTABLE: only the release of a hold can be recorded';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar09_hold_guard ON ar09_holds;
CREATE TRIGGER ar09_hold_guard BEFORE UPDATE OR DELETE ON ar09_holds FOR EACH ROW EXECUTE FUNCTION ar09_guard_hold();

-- A case's identity and its evidence-at-open never change; a terminal case is frozen.
CREATE OR REPLACE FUNCTION ar09_guard_case() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR09_CASE_NOT_DELETABLE: a collection case is never deleted';
    END IF;
    IF NEW.case_id <> OLD.case_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.customer_party_id <> OLD.customer_party_id OR NEW.currency <> OLD.currency OR NEW.aging_snapshot <> OLD.aging_snapshot
       OR NEW.policy_id <> OLD.policy_id THEN
        RAISE EXCEPTION 'AR09_CASE_IMMUTABLE: the customer, currency, pinned policy and aging snapshot of a case never change';
    END IF;
    IF OLD.state IN ('RESOLVED','CLOSED') THEN
        RAISE EXCEPTION 'AR09_CASE_TERMINAL: a resolved or closed case cannot change';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar09_case_guard ON ar09_collection_cases;
CREATE TRIGGER ar09_case_guard BEFORE UPDATE OR DELETE ON ar09_collection_cases FOR EACH ROW EXECUTE FUNCTION ar09_guard_case();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar09_dunning_policies','ar09_collection_cases','ar09_case_items','ar09_dunning_actions','ar09_promises','ar09_holds','ar09_history'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
