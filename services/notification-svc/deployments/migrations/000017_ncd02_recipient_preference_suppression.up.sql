-- ZS-SVC-Y-001 NCD-02: Recipient, Channel, Preference & Suppression (§5).
--
-- §5.3's separation rule is the shape of this migration: preference (a user's
-- convenience), suppression (a fact that forbids a route) and privacy/marketing
-- permission (PRV's decision, referenced, never stored as ours) live in
-- different tables and are combined only by the channel decision.

-- ── Preferences ─────────────────────────────────────────────────────────────
-- One current profile per principal, plus an append-only change history.
-- There is no consent column here, on purpose: POST /v1/preferences "cannot
-- mutate PRV consent or PDC rule".
CREATE TABLE IF NOT EXISTS ncd_preferences (
    tenant_id              VARCHAR(255) NOT NULL,
    principal_id           VARCHAR(255) NOT NULL,
    muted_channels         TEXT[]       NOT NULL DEFAULT '{}',
    channel_order          TEXT[]       NOT NULL DEFAULT '{}',
    quiet_hours_start      TIME,
    quiet_hours_end        TIME,
    -- IANA zone. Never guessed from a phone prefix or mail domain (NP-20).
    time_zone              VARCHAR(64),
    locale                 VARCHAR(20),
    version                INTEGER      NOT NULL DEFAULT 1,
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by_principal_id VARCHAR(255) NOT NULL,
    PRIMARY KEY (tenant_id, principal_id),
    CONSTRAINT ncd_pref_quiet_hours_pair CHECK ((quiet_hours_start IS NULL) = (quiet_hours_end IS NULL)),
    -- Quiet hours are civil time; without a zone they are meaningless (INV-23).
    CONSTRAINT ncd_pref_quiet_hours_need_zone CHECK (quiet_hours_start IS NULL OR time_zone IS NOT NULL),
    CONSTRAINT ncd_pref_channels_known CHECK (
        muted_channels <@ ARRAY['EMAIL','IN_APP','SMS','PUSH']::TEXT[]
        AND channel_order <@ ARRAY['EMAIL','IN_APP','SMS','PUSH']::TEXT[])
);

CREATE TABLE IF NOT EXISTS ncd_preference_changes (
    change_id              UUID         PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    principal_id           VARCHAR(255) NOT NULL,
    version                INTEGER      NOT NULL,
    snapshot               JSONB        NOT NULL,
    changed_by_principal_id VARCHAR(255) NOT NULL,
    changed_at             TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ncd_pref_changes ON ncd_preference_changes (tenant_id, principal_id, version);

-- ── Canonical suppression (§5.4, §7.3) ──────────────────────────────────────
-- Purpose- and channel-scoped. Keyed on a subject (principal) or an endpoint
-- HASH — never the raw address — so the list cannot be exported as a marketing
-- asset (§7.3) and matching still works across providers (NP-45: a complaint
-- at provider A still blocks traffic after a switch to provider B, because the
-- suppression belongs to NCD, not to the provider).
CREATE TABLE IF NOT EXISTS ncd_suppressions (
    suppression_id          UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    subject_principal_id    VARCHAR(255),
    endpoint_hash           VARCHAR(64),
    endpoint_masked         VARCHAR(255),
    channel_scope           VARCHAR(20)  NOT NULL DEFAULT 'ALL'
                                CHECK (channel_scope IN ('ALL','EMAIL','IN_APP','SMS','PUSH')),
    purpose_scope           VARCHAR(40)  NOT NULL DEFAULT 'ALL' CHECK (purpose_scope IN ('ALL',
                                'SECURITY_CRITICAL', 'REGULATED_RIGHTS_AFFECTING',
                                'TRANSACTIONAL_RELATIONSHIP', 'OPERATIONAL_WORKFLOW',
                                'SERVICE_INFORMATION', 'MARKETING_PROMOTIONAL')),
    reason                  VARCHAR(30)  NOT NULL CHECK (reason IN (
                                'HARD_BOUNCE','COMPLAINT_ABUSE','MARKETING_OPTOUT','CHANNEL_MUTE',
                                'SECURITY_HOLD','LEGAL_RESTRICTION','TEMP_SOFT_BOUNCE','ENDPOINT_INVALID')),
    source                  VARCHAR(30)  NOT NULL CHECK (source IN (
                                'PROVIDER_EVENT','RECIPIENT_UNSUBSCRIBE','OPERATOR','PRIVACY','POLICY','SECURITY','RECONCILIATION')),
    -- Mandatory: a suppression with no source evidence cannot later be
    -- defended or lifted (§10.1 "source and scope mandatory").
    source_evidence_ref     VARCHAR(500) NOT NULL,
    effective_from          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    effective_until         TIMESTAMPTZ,
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    lifted_at               TIMESTAMPTZ,
    lifted_by_principal_id  VARCHAR(255),
    lift_evidence_ref       VARCHAR(500),
    lift_approved_by_principal_id VARCHAR(255),
    CONSTRAINT ncd_supp_has_target CHECK (subject_principal_id IS NOT NULL OR endpoint_hash IS NOT NULL),
    CONSTRAINT ncd_supp_lift_has_evidence CHECK (lifted_at IS NULL OR (lift_evidence_ref IS NOT NULL AND lift_evidence_ref <> '' AND lifted_by_principal_id IS NOT NULL)),
    -- §7.3: reactivating a hard-bounced or complaint-suppressed endpoint needs
    -- governed evidence — "operator toggles alone are insufficient". The lift
    -- is approved by a second principal.
    CONSTRAINT ncd_supp_governed_reactivation CHECK (
        lifted_at IS NULL OR reason NOT IN ('HARD_BOUNCE','COMPLAINT_ABUSE','LEGAL_RESTRICTION','SECURITY_HOLD')
        OR (lift_approved_by_principal_id IS NOT NULL AND lift_approved_by_principal_id <> lifted_by_principal_id))
);

CREATE INDEX IF NOT EXISTS idx_ncd_supp_endpoint ON ncd_suppressions (tenant_id, endpoint_hash) WHERE lifted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_ncd_supp_subject ON ncd_suppressions (tenant_id, subject_principal_id) WHERE lifted_at IS NULL;
-- Idempotent processing (§7.3): the same fact from the same evidence is one row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ncd_supp_dedup ON ncd_suppressions
    (tenant_id, COALESCE(endpoint_hash, ''), COALESCE(subject_principal_id, ''), channel_scope, purpose_scope, reason, source_evidence_ref);

-- A suppression is a durable fact: rows are never deleted, and apart from the
-- governed lift columns nothing on one changes.
CREATE OR REPLACE FUNCTION ncd_reject_suppression_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ncd_suppressions rows are never deleted; lift them with evidence';
    END IF;
    IF NEW.suppression_id IS DISTINCT FROM OLD.suppression_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.subject_principal_id IS DISTINCT FROM OLD.subject_principal_id
        OR NEW.endpoint_hash IS DISTINCT FROM OLD.endpoint_hash
        OR NEW.channel_scope IS DISTINCT FROM OLD.channel_scope
        OR NEW.purpose_scope IS DISTINCT FROM OLD.purpose_scope
        OR NEW.reason IS DISTINCT FROM OLD.reason
        OR NEW.source IS DISTINCT FROM OLD.source
        OR NEW.source_evidence_ref IS DISTINCT FROM OLD.source_evidence_ref
        OR NEW.effective_from IS DISTINCT FROM OLD.effective_from
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'suppression % is immutable apart from its lift', OLD.suppression_id;
    END IF;
    IF OLD.lifted_at IS NOT NULL THEN
        RAISE EXCEPTION 'suppression % is already lifted', OLD.suppression_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ncd_reject_suppression_mutation ON ncd_suppressions;
CREATE TRIGGER trg_ncd_reject_suppression_mutation
    BEFORE UPDATE OR DELETE ON ncd_suppressions
    FOR EACH ROW EXECUTE FUNCTION ncd_reject_suppression_mutation();

-- ── Recipient plans (§5.1, §5.2) ────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ncd_recipient_plans (
    plan_id                 UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    intent_id               UUID         NOT NULL,
    intent_version          INTEGER      NOT NULL,
    recipient_principal_id  VARCHAR(255) NOT NULL,
    -- [{channel, endpoint, endpoint_hash, endpoint_masked, provenance,
    --   verified, provenance_ref, resolved_at}]
    endpoints               JSONB        NOT NULL,
    locale                  VARCHAR(20),
    time_zone               VARCHAR(64),
    time_zone_source        VARCHAR(30),
    decision_evidence       JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ncd_plans_tenant ON ncd_recipient_plans (tenant_id, created_at DESC);

-- ── Channel decisions (§5.5) ────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ncd_channel_decisions (
    decision_id             UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    plan_id                 UUID         NOT NULL REFERENCES ncd_recipient_plans (plan_id),
    intent_id               UUID         NOT NULL,
    intent_version          INTEGER      NOT NULL,
    communication_id        UUID,
    outcome                 VARCHAR(20)  NOT NULL CHECK (outcome IN ('PERMITTED','DEFERRED','BLOCKED','REVIEW_REQUIRED')),
    -- Ordered eligible routes: [{channel, endpoint_hash, binding_id, evidence_capability, reason}]
    routes                  JSONB        NOT NULL DEFAULT '[]'::jsonb,
    -- Excluded routes with their stable reason codes.
    restrictions            JSONB        NOT NULL DEFAULT '[]'::jsonb,
    reason_codes            TEXT[]       NOT NULL DEFAULT '{}',
    not_before              TIMESTAMPTZ,
    evidence_requirement    VARCHAR(2)   NOT NULL,
    fallback_rules          JSONB        NOT NULL DEFAULT '{}'::jsonb,
    -- Every input the decision read, so it can be replayed and defended.
    inputs                  JSONB        NOT NULL DEFAULT '{}'::jsonb,
    decided_by_principal_id VARCHAR(255) NOT NULL,
    decided_at              TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ncd_decisions_comm ON ncd_channel_decisions (tenant_id, communication_id, decided_at DESC);

-- Plans and decisions are evidence: append-only.
CREATE OR REPLACE FUNCTION ncd_reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% rows are append-only evidence', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ncd_plans_append_only ON ncd_recipient_plans;
CREATE TRIGGER trg_ncd_plans_append_only BEFORE UPDATE OR DELETE ON ncd_recipient_plans
    FOR EACH ROW EXECUTE FUNCTION ncd_reject_evidence_mutation();
DROP TRIGGER IF EXISTS trg_ncd_decisions_append_only ON ncd_channel_decisions;
CREATE TRIGGER trg_ncd_decisions_append_only BEFORE UPDATE OR DELETE ON ncd_channel_decisions
    FOR EACH ROW EXECUTE FUNCTION ncd_reject_evidence_mutation();
DROP TRIGGER IF EXISTS trg_ncd_pref_changes_append_only ON ncd_preference_changes;
CREATE TRIGGER trg_ncd_pref_changes_append_only BEFORE UPDATE OR DELETE ON ncd_preference_changes
    FOR EACH ROW EXECUTE FUNCTION ncd_reject_evidence_mutation();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ncd_preferences','ncd_preference_changes','ncd_suppressions','ncd_recipient_plans','ncd_channel_decisions']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_tenant', t);
        EXECUTE format('CREATE POLICY %I ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), ''''))
            WITH CHECK (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), ''''))', t || '_tenant', t);
    END LOOP;
END $$;
