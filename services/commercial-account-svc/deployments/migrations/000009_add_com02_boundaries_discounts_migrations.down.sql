-- 000009_add_com02_boundaries_discounts_migrations.down.sql
-- Refuses (constraint validation) if migrations have already been recorded;
-- removing them silently would destroy subscription history.

ALTER TABLE subscription_versions DROP CONSTRAINT subscription_versions_change_type_check;
ALTER TABLE subscription_versions ADD CONSTRAINT subscription_versions_change_type_check
    CHECK (change_type IN ('STARTED', 'TRIAL_CONVERSION', 'TRIAL_EXPIRY', 'ACTIVATED',
                           'CANCELLATION_SCHEDULED', 'CANCELLATION_EFFECTIVE', 'CANCELED_NOW',
                           'REACTIVATED', 'TERM_EXPIRY', 'PLAN_CHANGED', 'QUANTITY_CHANGED', 'ADD_ON_CHANGED'));

ALTER TABLE subscription_changes DROP CONSTRAINT IF EXISTS subscription_changes_one_basis;
ALTER TABLE subscription_changes DROP COLUMN IF EXISTS migration_offer_id;
ALTER TABLE subscription_changes ALTER COLUMN rule_id SET NOT NULL;
ALTER TABLE subscription_changes DROP CONSTRAINT subscription_changes_change_kind_check;
ALTER TABLE subscription_changes ADD CONSTRAINT subscription_changes_change_kind_check
    CHECK (change_kind IN ('PLAN_CHANGE', 'QUANTITY_CHANGE', 'ADD_ON_CHANGE'));

-- The enqueue triggers live on tables this rollback keeps; drop them before
-- their functions.
DROP TRIGGER IF EXISTS trg_subscription_versions_enqueue ON subscription_versions;
DROP TRIGGER IF EXISTS trg_subscription_terms_enqueue ON subscription_terms;

DROP TABLE IF EXISTS price_migration_offer_targets;
DROP TABLE IF EXISTS price_migration_offers;
DROP TABLE IF EXISTS subscription_discounts;
DROP TABLE IF EXISTS subscription_boundary_queue;

DROP FUNCTION IF EXISTS enforce_migration_target_draft_only();
DROP FUNCTION IF EXISTS enforce_migration_offer_lifecycle();
DROP FUNCTION IF EXISTS enforce_discount_lifecycle();
DROP FUNCTION IF EXISTS enqueue_term_boundary();
DROP FUNCTION IF EXISTS enqueue_version_boundary();
