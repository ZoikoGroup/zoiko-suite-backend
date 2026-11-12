// Package contracttest checks, from a producer's own test suite, that every
// event the service wrote to its eventing_outbox satisfies the payload schema
// the service publishes under contracts/events (ZS-EVENT-001 section 12.1, EV-T22).
//
// It exists so a producer that drifts from its published contract fails its own
// tests instead of failing a consumer in production. It reports problems as
// strings and leaves the decision to fail to the caller, because services run
// it from a TestMain after the whole suite, where there is no *testing.T.
package contracttest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/eventing/integration"
)

const typePrefix = "com.zoikosuite."

// maxReported bounds how many violations of one kind are listed, so a single
// broken schema does not drown the output of a 5,000-event suite.
const maxReported = 3

// CheckOutbox validates every event currently in eventing_outbox against
// <schemaDir>/<type minus com.zoikosuite.>.schema.json. It returns one message
// per distinct problem (empty when the outbox conforms) and the number of events
// checked, so a caller can refuse a vacuous pass.
//
// Events whose type does not start with com.zoikosuite. are skipped: they are
// legacy rows, not canonical events.
func CheckOutbox(ctx context.Context, pool *pgxpool.Pool, schemaDir string) (problems []string, checked int, err error) {
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('eventing_outbox') IS NOT NULL`).Scan(&present); err != nil {
		return nil, 0, fmt.Errorf("contracttest: probe outbox: %w", err)
	}
	if !present { // a test that never migrated has emitted nothing
		return nil, 0, nil
	}
	rows, err := pool.Query(ctx, `SELECT event_type, payload FROM eventing_outbox`)
	if err != nil {
		return nil, 0, fmt.Errorf("contracttest: read outbox: %w", err)
	}
	defer rows.Close()

	schemas := map[string][]byte{}
	missing := map[string]bool{}
	shown := map[string]int{}
	note := func(typ, msg string) {
		shown[typ]++
		if shown[typ] <= maxReported {
			problems = append(problems, typ+": "+msg)
		}
	}

	for rows.Next() {
		var typ string
		var payload []byte
		if err := rows.Scan(&typ, &payload); err != nil {
			return nil, checked, fmt.Errorf("contracttest: scan: %w", err)
		}
		if !strings.HasPrefix(typ, typePrefix) {
			continue
		}
		checked++
		if missing[typ] {
			continue
		}
		schema, ok := schemas[typ]
		if !ok {
			b, rerr := os.ReadFile(filepath.Join(schemaDir, strings.TrimPrefix(typ, typePrefix)+".schema.json"))
			if rerr != nil {
				missing[typ] = true
				problems = append(problems, typ+": emitted but has no published schema in "+schemaDir)
				continue
			}
			schema = b
			schemas[typ] = b
		}
		var wire struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(payload, &wire); err != nil || len(wire.Data) == 0 {
			note(typ, "outbox payload has no data member")
			continue
		}
		for _, v := range integration.ValidatePayload(schema, wire.Data) {
			note(typ, fmt.Sprint(v))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, checked, fmt.Errorf("contracttest: iterate outbox: %w", err)
	}
	for typ, n := range shown {
		if n > maxReported {
			problems = append(problems, fmt.Sprintf("%s: %d more violation(s) not shown", typ, n-maxReported))
		}
	}
	sort.Strings(problems)
	return problems, checked, nil
}

// Verify fails t for every event in the outbox that does not satisfy its
// published schema. Call it from a t.Cleanup registered after the pool's own
// Close cleanup (cleanups run last-in first-out), so it sees the rows the test
// wrote before the pool goes away. A test that emitted nothing passes: whether
// the suite exercises each event at all is the service's own coverage question.
func Verify(t testing.TB, pool *pgxpool.Pool, schemaDir string) {
	t.Helper()
	problems, _, err := CheckOutbox(context.Background(), pool, schemaDir)
	if err != nil {
		t.Errorf("event contract check: %v", err)
		return
	}
	for _, p := range problems {
		t.Errorf("event contract violation: %s", p)
	}
}
