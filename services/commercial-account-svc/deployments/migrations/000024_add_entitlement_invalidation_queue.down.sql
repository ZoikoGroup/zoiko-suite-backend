-- 000024_add_entitlement_invalidation_queue.down.sql
DROP TABLE IF EXISTS entitlement_invalidation_queue CASCADE;
DROP TRIGGER IF EXISTS trg_subscriptions_index_ends_at ON subscriptions;
DROP FUNCTION IF EXISTS index_subscription_ends_at() CASCADE;
DROP TABLE IF EXISTS ended_subscription_index CASCADE;
