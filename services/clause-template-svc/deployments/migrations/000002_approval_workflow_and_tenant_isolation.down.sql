-- +migrate Down
BEGIN;

ALTER TABLE contract_templates NO FORCE ROW LEVEL SECURITY;
ALTER TABLE clauses NO FORCE ROW LEVEL SECURITY;

ALTER TABLE contract_templates DROP COLUMN IF EXISTS approved_by;
ALTER TABLE contract_templates DROP COLUMN IF EXISTS approved_at;
ALTER TABLE contract_templates DROP CONSTRAINT IF EXISTS contract_templates_status_known;

DROP INDEX IF EXISTS idx_deviation_rules_tenant_entity;
DROP INDEX IF EXISTS idx_clause_versions_clause_id;

DROP TABLE IF EXISTS clause_deviation_rules;
DROP TABLE IF EXISTS clause_versions;

ALTER TABLE clauses DROP COLUMN IF EXISTS superseded_by;
ALTER TABLE clauses DROP COLUMN IF EXISTS retired_by;
ALTER TABLE clauses DROP COLUMN IF EXISTS retired_at;
ALTER TABLE clauses DROP COLUMN IF EXISTS activated_by;
ALTER TABLE clauses DROP COLUMN IF EXISTS activated_at;
ALTER TABLE clauses DROP COLUMN IF EXISTS approved_by;
ALTER TABLE clauses DROP COLUMN IF EXISTS approved_at;
ALTER TABLE clauses DROP COLUMN IF EXISTS submitted_by;
ALTER TABLE clauses DROP COLUMN IF EXISTS submitted_at;
ALTER TABLE clauses DROP COLUMN IF EXISTS authored_by_ai;

ALTER TABLE clauses DROP CONSTRAINT IF EXISTS clauses_status_known;

COMMIT;
