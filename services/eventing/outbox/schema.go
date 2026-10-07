package outbox

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TableName is the one outbox table every adopting service creates. A new
// name, deliberately distinct from the 24 hand-written variants
// (outbox_events, event_outbox, ...), so a service can adopt this package by
// adding a table rather than reshaping one whose old relay may still be
// running during a rolling deploy.
const TableName = "eventing_outbox"

// SchemaSQL is the DDL an adopting service copies verbatim into its own
// numbered migration (the migration runners only read each service's own
// deployments/migrations directory, so the DDL cannot live only here).
// VerifySchema checks at startup that it was applied.
//
// Columns follow ZS-EVENT-001 §6 one-for-one, plus three operational ones the
// standard's states imply: claimed_until (the lease that makes "claimed" safe
// across replicas), dispatched_at (the value stamped as publishedat) and
// last_error (human-readable detail beside the structured last_error_code).
//
// payload is BYTEA, not JSONB, on purpose: JSONB re-orders object keys, which
// would change the bytes payloadhash was computed over.
const SchemaSQL = `
CREATE TABLE eventing_outbox (
    outbox_id           UUID         PRIMARY KEY,
    event_id            TEXT         NOT NULL UNIQUE,
    event_type          TEXT         NOT NULL,
    schema_version      TEXT         NOT NULL,
    tenant_id           TEXT         NOT NULL,
    region              TEXT         NOT NULL,
    aggregate_id        TEXT,
    aggregate_version   BIGINT,
    partition_key       TEXT         NOT NULL,
    payload             BYTEA        NOT NULL,
    payload_hash        TEXT         NOT NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    publish_state       TEXT         NOT NULL DEFAULT 'pending',
    attempt_count       INTEGER      NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    claimed_until       TIMESTAMPTZ,
    dispatched_at       TIMESTAMPTZ,
    published_at        TIMESTAMPTZ,
    last_error_code     TEXT,
    last_error          TEXT,

    CONSTRAINT eventing_outbox_state_valid CHECK (publish_state IN
        ('pending', 'claimed', 'published', 'failed', 'quarantined')),
    CONSTRAINT eventing_outbox_attempts_nonneg CHECK (attempt_count >= 0),
    CONSTRAINT eventing_outbox_published_has_time CHECK
        (publish_state <> 'published' OR published_at IS NOT NULL),
    CONSTRAINT eventing_outbox_claimed_has_lease CHECK
        (publish_state <> 'claimed' OR claimed_until IS NOT NULL)
);

-- The dispatcher's claim scan: only rows that can still be delivered.
CREATE INDEX eventing_outbox_due_idx ON eventing_outbox (next_attempt_at, created_at)
    WHERE publish_state IN ('pending', 'failed');
CREATE INDEX eventing_outbox_lease_idx ON eventing_outbox (claimed_until)
    WHERE publish_state = 'claimed';
-- Backlog age and quarantine counts are first-class signals (ZS-EVENT-001 §6.1).
CREATE INDEX eventing_outbox_quarantined_idx ON eventing_outbox (created_at)
    WHERE publish_state = 'quarantined';
`

// requiredColumns is what this package reads and writes.
var requiredColumns = []string{
	"outbox_id", "event_id", "event_type", "schema_version", "tenant_id", "region",
	"aggregate_id", "aggregate_version", "partition_key", "payload", "payload_hash",
	"created_at", "publish_state", "attempt_count", "next_attempt_at", "claimed_until",
	"dispatched_at", "published_at", "last_error_code", "last_error",
}

// VerifySchema fails if the outbox table or any column this package uses is
// missing from the connection's search_path.
//
// Call it at startup next to the DB ping. Without it, a service whose
// migration was never applied starts fine and fails on the first business
// write that tries to enqueue an event — the write rolls back, so nothing is
// lost, but the outage surfaces on a user request instead of at deploy time.
func VerifySchema(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		 WHERE table_name = $1
		   AND table_schema = ANY (current_schemas(false))`, TableName)
	if err != nil {
		return fmt.Errorf("outbox: verify schema: %w", err)
	}
	defer rows.Close()

	have := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return fmt.Errorf("outbox: verify schema: %w", err)
		}
		have[c] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("outbox: verify schema: %w", err)
	}
	if len(have) == 0 {
		return fmt.Errorf("outbox: table %s does not exist — apply the eventing outbox migration", TableName)
	}
	var missing []string
	for _, c := range requiredColumns {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("outbox: table %s is missing columns: %s", TableName, strings.Join(missing, ", "))
	}
	return nil
}
