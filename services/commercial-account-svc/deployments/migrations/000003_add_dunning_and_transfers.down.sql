DROP TABLE IF EXISTS subscription_status_events;
DROP TABLE IF EXISTS billing_source_transfers;
ALTER TABLE commercial_subscriptions DROP COLUMN IF EXISTS billing_source;
