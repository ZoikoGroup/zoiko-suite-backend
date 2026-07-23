CREATE TABLE IF NOT EXISTS data_feed_subscriptions (
    feed_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    provider VARCHAR(128) NOT NULL,
    feed_type VARCHAR(64) NOT NULL,
    symbol VARCHAR(64),
    status VARCHAR(32) NOT NULL DEFAULT 'ACTIVE',
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_data_feeds_tenant_entity ON data_feed_subscriptions(tenant_id, legal_entity_id);

CREATE TABLE IF NOT EXISTS data_feed_events (
    event_id VARCHAR(64) PRIMARY KEY,
    feed_id VARCHAR(64) NOT NULL REFERENCES data_feed_subscriptions(feed_id) ON DELETE CASCADE,
    tenant_id VARCHAR(64) NOT NULL,
    event_type VARCHAR(128) NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    received_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_data_feed_events_feed ON data_feed_events(feed_id, received_at);
