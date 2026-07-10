-- Append-only per project doctrine: a schema version is never edited or
-- deleted once registered. Evolution always inserts a new (event_name,
-- version) row.
CREATE TABLE event_schemas (
    event_name      VARCHAR(255) NOT NULL,
    version         INT          NOT NULL,
    json_schema     JSONB        NOT NULL,
    registered_by   VARCHAR(255),
    registered_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (event_name, version)
);

CREATE INDEX idx_event_schemas_event_name ON event_schemas (event_name);
