-- Down migration for semantic-model-svc (DATA-05)
-- Migration: 000001_initial_schema.down.sql

DROP TRIGGER IF EXISTS trigger_semantic_version_lifecycle ON semantic_versions;
DROP FUNCTION IF EXISTS enforce_semantic_version_lifecycle();

DROP TRIGGER IF EXISTS trigger_metric_binding_retire_only ON metric_bindings;
DROP FUNCTION IF EXISTS enforce_metric_binding_retire_only();

DROP TRIGGER IF EXISTS trigger_calculation_plan_immutability ON calculation_plans;
DROP FUNCTION IF EXISTS enforce_calculation_plan_immutability();

DROP TRIGGER IF EXISTS trigger_dimension_binding_immutability ON dimension_bindings;
DROP FUNCTION IF EXISTS enforce_dimension_binding_immutability();

DROP TRIGGER IF EXISTS trigger_semantic_model_immutability ON semantic_models;
DROP FUNCTION IF EXISTS enforce_semantic_model_immutability();

DROP INDEX IF EXISTS idx_outbox_events_unpublished;
DROP INDEX IF EXISTS idx_calculation_plans_version;
DROP INDEX IF EXISTS idx_dimension_bindings_version;
DROP INDEX IF EXISTS idx_metric_bindings_key;
DROP INDEX IF EXISTS idx_metric_bindings_version;
DROP INDEX IF EXISTS idx_semantic_versions_status;
DROP INDEX IF EXISTS idx_semantic_versions_model;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
DROP POLICY IF EXISTS calculation_plans_delete ON calculation_plans;
DROP POLICY IF EXISTS calculation_plans_update ON calculation_plans;
DROP POLICY IF EXISTS calculation_plans_insert ON calculation_plans;
DROP POLICY IF EXISTS calculation_plans_read ON calculation_plans;
DROP POLICY IF EXISTS dimension_bindings_delete ON dimension_bindings;
DROP POLICY IF EXISTS dimension_bindings_update ON dimension_bindings;
DROP POLICY IF EXISTS dimension_bindings_insert ON dimension_bindings;
DROP POLICY IF EXISTS dimension_bindings_read ON dimension_bindings;
DROP POLICY IF EXISTS metric_bindings_delete ON metric_bindings;
DROP POLICY IF EXISTS metric_bindings_update ON metric_bindings;
DROP POLICY IF EXISTS metric_bindings_insert ON metric_bindings;
DROP POLICY IF EXISTS metric_bindings_read ON metric_bindings;
DROP POLICY IF EXISTS semantic_versions_delete ON semantic_versions;
DROP POLICY IF EXISTS semantic_versions_update ON semantic_versions;
DROP POLICY IF EXISTS semantic_versions_insert ON semantic_versions;
DROP POLICY IF EXISTS semantic_versions_read ON semantic_versions;
DROP POLICY IF EXISTS semantic_models_tenant_isolation ON semantic_models;

ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;
ALTER TABLE calculation_plans DISABLE ROW LEVEL SECURITY;
ALTER TABLE dimension_bindings DISABLE ROW LEVEL SECURITY;
ALTER TABLE metric_bindings DISABLE ROW LEVEL SECURITY;
ALTER TABLE semantic_versions DISABLE ROW LEVEL SECURITY;
ALTER TABLE semantic_models DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS calculation_plans;
DROP TABLE IF EXISTS dimension_bindings;
DROP TABLE IF EXISTS metric_bindings;
DROP TABLE IF EXISTS semantic_versions;
DROP TABLE IF EXISTS semantic_models;
