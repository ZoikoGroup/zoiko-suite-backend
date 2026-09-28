-- Down migration for 000008_bounce_complaint_reputation.up.sql

DROP POLICY IF EXISTS channel_reputation_tenant_isolation ON channel_reputation;
DROP TABLE IF EXISTS channel_reputation;

DROP POLICY IF EXISTS complaint_events_tenant_isolation ON complaint_events;
DROP TABLE IF EXISTS complaint_events;

DROP POLICY IF EXISTS bounce_events_tenant_isolation ON bounce_events;
DROP TABLE IF EXISTS bounce_events;