-- 000001_initial_schema.up.sql
-- Kill Switch Registry Service — initial schema
--
-- docs/original_doc/zoiko_suite_doc7.txt §32.1: "Granular controls for
-- commercial charging, high-impact automation/tool actions, model/provider
-- use, obligation/rule activation, selected imports/syncs, outbound
-- notifications/exports and public claim publication... Kill switch is
-- plane/domain/provider/tenant scoped, privileged, logged, approval-
-- controlled, visible in operations, and has reconciliation/restart
-- procedure. It does not erase already-committed history."
--
-- Distinct from capability-registry-svc's `releases` table: that answers
-- "is this capability available in this market" (a product/marketing
-- question, capability_id-scoped only). This answers "must this class of
-- action be stopped right now, platform-wide or narrower" (an incident-
-- response question), scoped across four independent dimensions at once.
--
-- Owns:
--   kill_switch_events — append-only ENGAGE/DISENGAGE log. "Currently
--     engaged" for any (plane, domain, provider_code, tenant_id) tuple is
--     DERIVED as that tuple's most recent event — never an UPDATE-in-place
--     status column, so the doctrine's "does not erase already-committed
--     history" is structural, not just a convention callers must honor.
--
-- plane/domain are VARCHAR — DATA ONLY, no code switch/case in this
-- service. domain's value set is the 7 action classes doc7 §32.1 names
-- verbatim (COMMERCIAL_CHARGING, AUTOMATION_ACTION, MODEL_PROVIDER_USE,
-- OBLIGATION_RULE_ACTIVATION, IMPORT_SYNC, NOTIFICATION_EXPORT,
-- PUBLIC_CLAIM_PUBLICATION) — enforced by callers choosing sensible values,
-- not a DB CHECK constraint, so a new action class never requires a
-- migration.
--
-- provider_code/tenant_id are nullable: NULL on either means "not scoped
-- to one provider" / "not scoped to one tenant" respectively — the same
-- nullable-scope doctrine used throughout this codebase (policy_versions'
-- tenant_id/legal_entity_id, workspaces' billing_source). plane is also
-- nullable: NULL means "applies regardless of plane" — the most severe,
-- broadest kill switch doc7 describes ("stopping new harm must not require
-- taking the whole platform offline", so plane+domain=NULL+NULL is
-- deliberately possible but never the only option).

CREATE TABLE kill_switch_events (
    kill_switch_event_id      UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- event_seq is the true ordering key for "what is the latest event for
    -- this scope" — created_at is wall-clock time for display/audit only.
    -- Two engage/disengage calls in rapid succession (a real possibility
    -- during an incident, scripted or human) can land in the same
    -- microsecond; a BIGSERIAL never ties.
    event_seq                  BIGSERIAL   NOT NULL,

    -- NULL = applies across all planes.
    plane                       VARCHAR(64),
    -- NULL = a true platform-wide switch, not one action class. Required
    -- in practice by the handler for anything less than a full platform
    -- stop — see internal/handler's missingField validation.
    domain                        VARCHAR(64),
    -- NULL = not provider-specific.
    provider_code                   VARCHAR(128),
    -- NULL = not tenant-specific (platform-wide for whatever plane/domain
    -- scope IS set).
    tenant_id                          UUID,

    -- ENGAGE | DISENGAGE — DATA ONLY.
    action                                VARCHAR(16) NOT NULL,

    reason                                   TEXT        NOT NULL,

    -- Required on ENGAGE (doc7's "has reconciliation/restart procedure") —
    -- a reference to the runbook, not the runbook content itself; this
    -- service records where to look, not what to do, same doctrine as
    -- capability_claims never inventing the marketing copy it references.
    reconciliation_procedure_ref              TEXT,

    -- Approval-controlled: the principal who authorized this specific
    -- action, which may or may not be the same as created_by_principal_id
    -- depending on who is privileged to self-authorize an emergency stop —
    -- doc7 does not mandate maker-checker here the way §G3 does for
    -- AI-proposed policy changes, so this service does not invent that
    -- constraint.
    approved_by_principal_id                     TEXT        NOT NULL,

    created_at                                      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id                            TEXT        NOT NULL
);

-- Primary lookup: latest event for a given scope tuple, used by both the
-- resolve endpoint (fallback matching across specificity tiers) and the
-- operations-visibility list endpoint. NULLS are grouped together by
-- DISTINCT ON, so this also serves "all events for the platform-wide
-- tuple" (all four columns NULL) correctly.
CREATE INDEX idx_kill_switch_events_scope
    ON kill_switch_events (plane, domain, provider_code, tenant_id, event_seq DESC);
