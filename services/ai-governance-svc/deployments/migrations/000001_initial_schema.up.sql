-- 000001_initial_schema.up.sql
-- ai-governance-svc — doc7 §11 (AI, Agentic Automation & Human Authority).

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ai_runs: doc7 §G1's AI run/recommendation object.
CREATE TABLE ai_runs (
    ai_run_id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    run_type                 VARCHAR(32) NOT NULL,
    model_id                 VARCHAR(255) NOT NULL,
    prompt_version            VARCHAR(64) NOT NULL,
    tool_version               VARCHAR(64),
    source_refs                  JSONB NOT NULL DEFAULT '[]'::jsonb,
    evidence_refs                  JSONB NOT NULL DEFAULT '[]'::jsonb,
    confidence                        NUMERIC(5,4),
    limitation                          TEXT,
    uncertainty_state                     VARCHAR(32) NOT NULL DEFAULT 'NONE',
    recommended_action                       TEXT,
    audit_id                                    VARCHAR(255) NOT NULL,
    created_at                                     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id                           VARCHAR(255) NOT NULL
);

CREATE INDEX idx_ai_runs_tenant ON ai_runs (tenant_id, created_at DESC);

-- action_risk_classifications: doc7 §G2's risk taxonomy + human-review
-- trigger. ActionType is data — every consumer of this table looks it up,
-- none of them switch on it here.
CREATE TABLE action_risk_classifications (
    action_type              VARCHAR(128) PRIMARY KEY,
    risk_category               VARCHAR(32) NOT NULL DEFAULT 'NONE',
    human_review_trigger           BOOLEAN NOT NULL DEFAULT FALSE,
    requires_maker_checker            BOOLEAN NOT NULL DEFAULT FALSE,
    created_at                           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id                 VARCHAR(255) NOT NULL
);

-- automation_policies: doc7 §G7's autonomous-action allowlist, scoped per
-- tenant/role/risk-class/tool/action — fail-closed by default (absence of a
-- row means the action is not allowed).
CREATE TABLE automation_policies (
    automation_policy_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    role                        VARCHAR(128) NOT NULL,
    risk_category                  VARCHAR(32) NOT NULL DEFAULT 'NONE',
    tool                              VARCHAR(128) NOT NULL,
    action_type                          VARCHAR(128) NOT NULL,
    max_scope_amount                         NUMERIC(18,2),
    required_approvals                          INT NOT NULL DEFAULT 0,
    dry_run_required                               BOOLEAN NOT NULL DEFAULT FALSE,
    rate_limit_per_day                                INT,
    kill_switch_engaged                                  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at                                              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id                                    VARCHAR(255) NOT NULL,
    UNIQUE (tenant_id, role, risk_category, tool, action_type)
);

CREATE INDEX idx_automation_policies_lookup ON automation_policies (tenant_id, role, risk_category, tool, action_type);

-- automation_actions: doc7 §G2/§G7's automation_action object — one row per
-- proposed or executed autonomous action.
CREATE TABLE automation_actions (
    automation_action_id     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    action_type                 VARCHAR(128) NOT NULL,
    risk_category                  VARCHAR(32) NOT NULL DEFAULT 'NONE',
    idempotency_key                    VARCHAR(255) NOT NULL,
    preconditions_met                     BOOLEAN NOT NULL DEFAULT FALSE,
    approval_status                          VARCHAR(32) NOT NULL DEFAULT 'NOT_REQUIRED',
    postcondition_verified                      BOOLEAN NOT NULL DEFAULT FALSE,
    rollback_plan                                  TEXT,
    status                                            VARCHAR(32) NOT NULL DEFAULT 'PROPOSED',
    proposed_by_principal_id                            VARCHAR(255) NOT NULL,
    approved_by_principal_id                               VARCHAR(255),
    created_at                                                TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at                                                   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, idempotency_key)
);

CREATE INDEX idx_automation_actions_tenant ON automation_actions (tenant_id, created_at DESC);

-- model_provider_registrations: doc7 §G6's provider/model registry.
CREATE TABLE model_provider_registrations (
    provider_registration_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_name              VARCHAR(128) NOT NULL,
    model_name                    VARCHAR(128) NOT NULL,
    training_use_posture              VARCHAR(32) NOT NULL DEFAULT 'NO_TRAINING',
    retention_policy_ref                 VARCHAR(255),
    data_region                             VARCHAR(64) NOT NULL,
    dpa_verified                               BOOLEAN NOT NULL DEFAULT FALSE,
    approved_data_classes                         JSONB NOT NULL DEFAULT '[]'::jsonb,
    approved_at                                      TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id                            VARCHAR(255),
    created_at                                             TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE (provider_name, model_name)
);

-- policy_change_approvals: doc7 §G3/§H3's maker-checker gate for
-- AI-proposed policy or control changes, with mandatory self-approval
-- blocking enforced at decision time (application layer, not just a
-- constraint, since the decider identity isn't known at insert time).
CREATE TABLE policy_change_approvals (
    policy_change_approval_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    target_policy_ref           VARCHAR(255) NOT NULL,
    proposed_change                TEXT NOT NULL,
    proposed_by_principal_id          VARCHAR(255) NOT NULL,
    decision                             VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    decided_by_principal_id                 VARCHAR(255),
    decision_reason                            TEXT,
    decided_at                                    TIMESTAMP WITH TIME ZONE,
    created_at                                       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_policy_change_approvals_target ON policy_change_approvals (target_policy_ref, created_at DESC);
