-- 000007_add_com02_subscriptions.down.sql

DROP TRIGGER IF EXISTS trg_commercial_subscriptions_com02_exclusion ON commercial_subscriptions;

DROP TABLE IF EXISTS subscription_item_quantities;
DROP TABLE IF EXISTS subscription_items;
DROP TABLE IF EXISTS subscription_terms;
DROP TABLE IF EXISTS subscription_versions;
DROP TABLE IF EXISTS subscriptions;

DROP FUNCTION IF EXISTS reject_subscription_row_mutation();
DROP FUNCTION IF EXISTS enforce_void_only_update();
DROP FUNCTION IF EXISTS reject_legacy_subscription_over_com02();
DROP FUNCTION IF EXISTS enforce_subscription_update();
DROP FUNCTION IF EXISTS enforce_subscription_insert();

ALTER TABLE commercial_accounts
    DROP CONSTRAINT IF EXISTS commercial_accounts_market_evidence,
    DROP COLUMN IF EXISTS market_set_by_principal_id,
    DROP COLUMN IF EXISTS market_set_at,
    DROP COLUMN IF EXISTS market_code;

-- btree_gist is left installed: other schemas in this database may use it.
