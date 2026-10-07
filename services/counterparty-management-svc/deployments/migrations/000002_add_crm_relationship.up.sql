-- +migrate Up
BEGIN;

-- BIZ-06 CRM / Relationship lives beside Counterparty in this same
-- service — the user's own decision: Counterparty already owns external
-- party identity (including a CUSTOMER type), which is exactly the "ORG
-- party identity" BIZ-06 is meant to reference, not own. TEXT ids
-- ("rel-<uuid>", "opp-<uuid>", ...) to match this service's own existing
-- convention, not native UUID columns.
--
-- Relationship references counterparty_id but does NOT require one at
-- creation — the doc's own failure semantics: "duplicate party match
-- remains unresolved candidate until ORG linkage confirmed." A
-- relationship may carry a candidate_counterparty_id (a tentative,
-- unconfirmed match) separately from counterparty_id (the confirmed
-- link), and ConfirmPartyLink (a gap-fill; see internal/domain/crm.go's
-- own doc comment) is the only way a candidate becomes confirmed.

CREATE TABLE relationships (
    relationship_id           TEXT        NOT NULL,
    tenant_id                  TEXT        NOT NULL,
    legal_entity_id             TEXT        NOT NULL,
    -- Confirmed link only — NULL until ConfirmPartyLink runs.
    counterparty_id               TEXT,
    -- Tentative, unconfirmed match — never auto-promoted to counterparty_id.
    candidate_counterparty_id       TEXT,
    party_link_status                 TEXT        NOT NULL DEFAULT 'UNLINKED'
        CHECK (party_link_status IN ('UNLINKED', 'CANDIDATE', 'CONFIRMED')),
    status                              TEXT        NOT NULL DEFAULT 'PROSPECT'
        CHECK (status IN ('PROSPECT', 'ACTIVE', 'DORMANT', 'CLOSED', 'MERGED')),
    source                                TEXT        NOT NULL DEFAULT '',
    channel                                 TEXT        NOT NULL DEFAULT '',
    owner_principal_id                        TEXT        NOT NULL DEFAULT '',
    merged_into_relationship_id                 TEXT,
    created_by                                    TEXT        NOT NULL,
    created_at                                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_by                                           TEXT        NOT NULL DEFAULT '',
    closed_at                                             TIMESTAMPTZ,
    closure_reason                                          TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (relationship_id, tenant_id)
);
CREATE INDEX idx_relationships_tenant_entity ON relationships (tenant_id, legal_entity_id);
CREATE INDEX idx_relationships_status ON relationships (tenant_id, status);
CREATE INDEX idx_relationships_counterparty ON relationships (tenant_id, counterparty_id) WHERE counterparty_id IS NOT NULL;
CREATE INDEX idx_relationships_owner ON relationships (tenant_id, owner_principal_id) WHERE owner_principal_id <> '';

-- Append-only — one row per logged touchpoint, forming the timeline
-- GetTimeline reads.
CREATE TABLE interactions (
    interaction_id     TEXT        NOT NULL,
    relationship_id      TEXT        NOT NULL,
    tenant_id               TEXT        NOT NULL,
    interaction_type          TEXT        NOT NULL,
    channel                     TEXT        NOT NULL DEFAULT '',
    notes                         TEXT        NOT NULL DEFAULT '',
    occurred_at                     TIMESTAMPTZ NOT NULL DEFAULT now(),
    logged_by                         TEXT        NOT NULL,
    created_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (interaction_id, tenant_id)
);
CREATE INDEX idx_interactions_relationship ON interactions (tenant_id, relationship_id, occurred_at);

-- Fixed, validated stage set — not a separately governed
-- pipeline-definition entity. See internal/domain/crm.go's own doc
-- comment on why: the doc's command list names no command that manages
-- pipeline-stage definitions, only UpdateStage (moving an opportunity
-- along the pipeline). "Versioned" is satisfied honestly via
-- opportunity_stage_transitions' full append-only history instead of a
-- fabricated stage-governance subsystem.
CREATE TABLE opportunities (
    opportunity_id      TEXT        NOT NULL,
    relationship_id        TEXT        NOT NULL,
    tenant_id                 TEXT        NOT NULL,
    legal_entity_id             TEXT        NOT NULL,
    name                          TEXT        NOT NULL,
    value                           NUMERIC(20, 2) NOT NULL DEFAULT 0,
    currency                          TEXT        NOT NULL DEFAULT 'USD',
    stage                               TEXT        NOT NULL DEFAULT 'LEAD'
        CHECK (stage IN ('LEAD', 'QUALIFIED', 'PROPOSAL', 'NEGOTIATION', 'CLOSED_WON', 'CLOSED_LOST')),
    status                                 TEXT        NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'CLOSED')),
    owner_principal_id                       TEXT        NOT NULL DEFAULT '',
    created_by                                 TEXT        NOT NULL,
    created_at                                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                                     TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_by                                        TEXT        NOT NULL DEFAULT '',
    closed_at                                          TIMESTAMPTZ,
    close_reason                                         TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (opportunity_id, tenant_id)
);
CREATE INDEX idx_opportunities_relationship ON opportunities (tenant_id, relationship_id);
CREATE INDEX idx_opportunities_tenant_entity_stage ON opportunities (tenant_id, legal_entity_id, stage) WHERE status = 'OPEN';

-- Append-only — one row per UpdateStage call, the real "versioned"
-- record of an opportunity's movement through the pipeline.
CREATE TABLE opportunity_stage_transitions (
    transition_id        TEXT        NOT NULL,
    opportunity_id          TEXT        NOT NULL,
    tenant_id                 TEXT        NOT NULL,
    from_stage                  TEXT        NOT NULL,
    to_stage                      TEXT        NOT NULL,
    actor_principal_id              TEXT        NOT NULL,
    reason                             TEXT        NOT NULL DEFAULT '',
    occurred_at                          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (transition_id, tenant_id)
);
CREATE INDEX idx_opportunity_stage_transitions_opp ON opportunity_stage_transitions (tenant_id, opportunity_id, occurred_at);

-- Append-only — each consent decision is a new row; GetConsentContext
-- reads the latest per consent_type. Never overwritten, so a withdrawal
-- is provable evidence, not a silently-lost prior state.
CREATE TABLE consent_records (
    consent_id       TEXT        NOT NULL,
    relationship_id     TEXT        NOT NULL,
    tenant_id              TEXT        NOT NULL,
    consent_type             TEXT        NOT NULL,
    consent_status              TEXT        NOT NULL CHECK (consent_status IN ('GRANTED', 'WITHDRAWN')),
    basis                          TEXT        NOT NULL DEFAULT '',
    recorded_by                       TEXT        NOT NULL,
    recorded_at                         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consent_id, tenant_id)
);
CREATE INDEX idx_consent_records_relationship ON consent_records (tenant_id, relationship_id, consent_type, recorded_at DESC);

-- Opaque cross-service reference — no FK, same posture as
-- document-vault-svc's document_links and exception-escalation-svc's
-- linked_object_type/linked_object_id.
CREATE TABLE commercial_object_links (
    link_id               TEXT        NOT NULL,
    relationship_id          TEXT        NOT NULL,
    tenant_id                   TEXT        NOT NULL,
    linked_object_type            TEXT        NOT NULL,
    linked_object_id                TEXT        NOT NULL,
    linked_by                          TEXT        NOT NULL,
    linked_at                            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (link_id, tenant_id),
    UNIQUE (relationship_id, tenant_id, linked_object_type, linked_object_id)
);
CREATE INDEX idx_commercial_object_links_relationship ON commercial_object_links (tenant_id, relationship_id);

ALTER TABLE relationships ENABLE ROW LEVEL SECURITY;
ALTER TABLE relationships FORCE ROW LEVEL SECURITY;
CREATE POLICY relationships_tenant_isolation ON relationships
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE interactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE interactions FORCE ROW LEVEL SECURITY;
CREATE POLICY interactions_tenant_isolation ON interactions
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE opportunities ENABLE ROW LEVEL SECURITY;
ALTER TABLE opportunities FORCE ROW LEVEL SECURITY;
CREATE POLICY opportunities_tenant_isolation ON opportunities
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE opportunity_stage_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE opportunity_stage_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY opportunity_stage_transitions_tenant_isolation ON opportunity_stage_transitions
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE consent_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE consent_records FORCE ROW LEVEL SECURITY;
CREATE POLICY consent_records_tenant_isolation ON consent_records
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE commercial_object_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_object_links FORCE ROW LEVEL SECURITY;
CREATE POLICY commercial_object_links_tenant_isolation ON commercial_object_links
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Append-only doctrine, same trigger shape used throughout this build.
CREATE OR REPLACE FUNCTION reject_interaction_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'interactions are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_interaction_mutation
    BEFORE UPDATE OR DELETE ON interactions
    FOR EACH ROW EXECUTE FUNCTION reject_interaction_mutation();

CREATE OR REPLACE FUNCTION reject_opportunity_stage_transition_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'opportunity stage transitions are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_opportunity_stage_transition_mutation
    BEFORE UPDATE OR DELETE ON opportunity_stage_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_opportunity_stage_transition_mutation();

CREATE OR REPLACE FUNCTION reject_consent_record_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'consent records are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_consent_record_mutation
    BEFORE UPDATE OR DELETE ON consent_records
    FOR EACH ROW EXECUTE FUNCTION reject_consent_record_mutation();

CREATE OR REPLACE FUNCTION reject_commercial_object_link_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'commercial object links are append-only';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_reject_commercial_object_link_mutation
    BEFORE UPDATE OR DELETE ON commercial_object_links
    FOR EACH ROW EXECUTE FUNCTION reject_commercial_object_link_mutation();

COMMIT;
