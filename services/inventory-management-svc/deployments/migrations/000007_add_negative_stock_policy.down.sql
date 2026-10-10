ALTER TABLE inventory_tracking_policies
    DROP CONSTRAINT IF EXISTS chk_inventory_negative_stock_policy,
    DROP COLUMN IF EXISTS negative_stock_policy;
