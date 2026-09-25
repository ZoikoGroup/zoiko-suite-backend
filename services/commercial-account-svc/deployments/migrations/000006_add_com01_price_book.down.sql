-- 000006_add_com01_price_book.down.sql

-- products_read's subquery depends on product_price_versions, which cannot be
-- dropped while that policy exists. Named explicitly rather than CASCADE, so
-- a dependency nobody expected fails the rollback instead of vanishing.
DROP POLICY IF EXISTS products_read ON commercial_products;

DROP TABLE IF EXISTS commercial_idempotency_keys;
DROP TABLE IF EXISTS price_version_capabilities;
DROP TABLE IF EXISTS price_component_tiers;
DROP TABLE IF EXISTS price_components;
DROP TABLE IF EXISTS product_price_versions;
DROP TABLE IF EXISTS commercial_products;
DROP TABLE IF EXISTS commercial_currencies;

DROP FUNCTION IF EXISTS reject_idempotency_key_update();
DROP FUNCTION IF EXISTS enforce_price_child_draft_only();
DROP FUNCTION IF EXISTS enforce_price_version_lifecycle();
DROP FUNCTION IF EXISTS reject_commercial_product_mutation();
DROP FUNCTION IF EXISTS enforce_commercial_currency_immutability();
