-- 000020_add_com01_publisher_sod.down.sql

ALTER TABLE product_price_versions
    DROP CONSTRAINT IF EXISTS price_versions_publisher_is_independent;
