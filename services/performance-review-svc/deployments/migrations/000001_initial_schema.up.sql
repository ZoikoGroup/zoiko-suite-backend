-- Migration 000001: Initial schema for performance-review-svc
-- Phase 4 — Workforce Engine
-- Doc ref: docs/architecture/04-data-model.md §10.1 (PerformanceReview entity)
-- Doctrine: every material record carries tenant_id, legal_entity_id, effective_from/effective_to

-- ── Review Cycles ─────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS review_cycles (
    review_cycle_id   UUID                     PRIMARY KEY,
    tenant_id         VARCHAR(255)             NOT NULL,  -- RLS isolation key
    legal_entity_id   VARCHAR(255)             NOT NULL,  -- entity scope (doctrine §3.2)
    cycle_name        VARCHAR(255)             NOT NULL,
    cycle_type        VARCHAR(64)              NOT NULL,  -- ANNUAL | SEMI_ANNUAL | PROBATIONARY | PROJECT_BASED
    start_date        DATE                     NOT NULL,
    end_date          DATE                     NOT NULL,
    cycle_status      VARCHAR(32)              NOT NULL DEFAULT 'DRAFT', -- DRAFT | ACTIVE | IN_EVALUATION | COMPLETED | ARCHIVED
    effective_from    TIMESTAMP WITH TIME ZONE NOT NULL,  -- doctrine: all material records
    effective_to      TIMESTAMP WITH TIME ZONE,           -- doctrine: end-dating only, no hard delete
    created_at        TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at        TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE review_cycles ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Isolation Policy
CREATE POLICY tenant_isolation_policy ON review_cycles FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX idx_review_cycles_tenant_entity  ON review_cycles (tenant_id, legal_entity_id);
CREATE INDEX idx_review_cycles_tenant_status  ON review_cycles (tenant_id, cycle_status);
CREATE INDEX idx_review_cycles_tenant_type    ON review_cycles (tenant_id, cycle_type);

-- ── Performance Reviews ───────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS performance_reviews (
    performance_review_id   UUID                     PRIMARY KEY,
    tenant_id               VARCHAR(255)             NOT NULL,  -- RLS isolation key
    legal_entity_id         VARCHAR(255)             NOT NULL,  -- entity scope (doctrine §3.2)
    review_cycle_id         UUID                     NOT NULL REFERENCES review_cycles(review_cycle_id),
    employee_id             UUID                     NOT NULL,  -- opaque ref → employee-master-svc
    reviewer_principal_id   UUID                     NOT NULL,  -- opaque ref → Principal (identity-context-svc)
    review_status           VARCHAR(32)              NOT NULL DEFAULT 'INITIATED',
    -- INITIATED | SELF_ASSESSMENT_PENDING | MANAGER_REVIEW_PENDING | SUBMITTED | APPROVED | COMPLETED | CANCELLED
    overall_rating          NUMERIC(3,2),                       -- nullable until SUBMITTED; 0.00–5.00
    self_assessment_payload JSONB,                              -- nullable; employee self-review content
    manager_eval_payload    JSONB,                              -- nullable; manager evaluation content
    governance_decision_id  UUID,                               -- written when COMPLETED; links to governance-decision-log-svc
    idempotency_key         VARCHAR(255),                       -- optional client-supplied duplicate suppression key
    completed_at            TIMESTAMP WITH TIME ZONE,           -- immutable once set; terminal state timestamp
    effective_from          TIMESTAMP WITH TIME ZONE NOT NULL,  -- doctrine: all material records
    effective_to            TIMESTAMP WITH TIME ZONE,           -- doctrine: end-dating only, no hard delete
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at              TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE performance_reviews ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Isolation Policy
CREATE POLICY tenant_isolation_policy ON performance_reviews FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Idempotency: unique key per tenant (partial index — only when key is provided)
CREATE UNIQUE INDEX idx_reviews_idempotency_key ON performance_reviews (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Query Indexes
CREATE INDEX idx_reviews_tenant_employee   ON performance_reviews (tenant_id, employee_id);
CREATE INDEX idx_reviews_tenant_cycle      ON performance_reviews (tenant_id, review_cycle_id);
CREATE INDEX idx_reviews_tenant_status     ON performance_reviews (tenant_id, review_status);
CREATE INDEX idx_reviews_tenant_reviewer   ON performance_reviews (tenant_id, reviewer_principal_id);
CREATE INDEX idx_reviews_tenant_entity     ON performance_reviews (tenant_id, legal_entity_id);
