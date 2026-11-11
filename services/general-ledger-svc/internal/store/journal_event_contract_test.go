package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/eventing/integration"
	"zoiko.io/general-ledger-svc/internal/domain"
	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
	"zoiko.io/general-ledger-svc/internal/store"
)

const journalPrefix = "com.zoikosuite.accounting.journal."

func journalSchemaDir() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "contracts", "events")
}

// assertEventsMatchSchemas validates every event whose type starts with typePrefix in the outbox
// against the published schema of its type (ZS-EVENT-001 section 12.1) and
// returns the set of types it saw, so a test can assert its own coverage.
func assertEventsMatchSchemas(t *testing.T, pool *pgxpool.Pool, typePrefix string) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT event_type, payload FROM eventing_outbox WHERE event_type LIKE $1`, typePrefix+"%")
	require.NoError(t, err)
	defer rows.Close()
	seen := map[string]int{}
	for rows.Next() {
		var typ string
		var payload []byte
		require.NoError(t, rows.Scan(&typ, &payload))
		schema, err := os.ReadFile(filepath.Join(journalSchemaDir(), strings.TrimPrefix(typ, "com.zoikosuite.")+".schema.json"))
		require.NoError(t, err, "every emitted journal event type needs a contracts/events schema: %s", typ)
		var wire struct {
			DataSchema string          `json:"dataschema"`
			Data       json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(payload, &wire))
		assert.Equal(t, "urn:zoikosuite:schema:"+strings.TrimPrefix(typ, "com.zoikosuite.")+":1.0.0", wire.DataSchema)
		assert.Empty(t, integration.ValidatePayload(schema, wire.Data), "%s: %s", typ, wire.Data)
		seen[typ]++
	}
	require.NoError(t, rows.Err())
	return seen
}

func newTestJournal(tenantID, legalEntityID string, approval domain.ApprovalStatus) (*domain.JournalHeader, []domain.JournalLine) {
	d := domain.NewDate(2026, time.August, 1)
	return &domain.JournalHeader{
		JournalID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID, FiscalPeriod: "2026-08",
		Status: domain.JournalStatusPending, TransactionDate: d, PostingDate: d, CreatedByPrincipalID: "preparer-1",
		CorrelationID: uuid.New().String(), ApprovalStatus: approval,
	}, []domain.JournalLine{
		{AccountCode: "1000", DebitAmount: 100},
		{AccountCode: "2000", CreditAmount: 100},
	}
}

// Every journal lifecycle event the store emits on real Postgres satisfies its
// published payload schema, and all eight lifecycle facts are exercised.
func TestPgStore_JournalLifecycleEventsSatisfyPublishedSchemas_RealDB(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop(), store.WithEventRegion("uk"))
	tenantID := uuid.New().String()
	entity := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	create := func(approval domain.ApprovalStatus) *domain.JournalHeader {
		h, lines := newTestJournal(tenantID, entity, approval)
		_, _, err := s.CreateJournal(ctx, h, lines)
		require.NoError(t, err)
		return h
	}

	// created -> proposed -> approved
	a := create(domain.ApprovalStatusDraft)
	require.NoError(t, s.SubmitJournalForApproval(ctx, tenantID, a.JournalID, "preparer-1"))
	require.NoError(t, s.ApproveJournal(ctx, tenantID, a.JournalID, "approver-1", "fp-1"))

	// created -> proposed -> rejected
	b := create(domain.ApprovalStatusDraft)
	require.NoError(t, s.SubmitJournalForApproval(ctx, tenantID, b.JournalID, "preparer-1"))
	require.NoError(t, s.RejectJournal(ctx, tenantID, b.JournalID, "approver-1", "wrong period"))

	// created -> withdrawn
	c := create(domain.ApprovalStatusDraft)
	require.NoError(t, s.WithdrawJournal(ctx, tenantID, c.JournalID, "preparer-1", "entered twice"))

	// created -> validated -> posted -> reversed
	d := create(domain.ApprovalStatusPosted)
	require.NoError(t, s.TransitionJournal(ctx, tenantID, d.JournalID, domain.JournalStatusPending, domain.JournalStatusValidated, "v1"))
	require.NoError(t, s.TransitionJournal(ctx, tenantID, d.JournalID, domain.JournalStatusValidated, domain.JournalStatusFinalized, "p1"))
	rev, revLines := newTestJournal(tenantID, entity, domain.ApprovalStatusPosted)
	rev.Status = domain.JournalStatusFinalized
	rev.ReversalOfJournalID = &d.JournalID
	rev.Description = "Reversal"
	revLines = []domain.JournalLine{{AccountCode: "1000", CreditAmount: 100}, {AccountCode: "2000", DebitAmount: 100}}
	_, created, err := s.ReverseJournal(ctx, tenantID, d.JournalID, rev, revLines, "reverser-1")
	require.NoError(t, err)
	require.True(t, created)

	seen := assertEventsMatchSchemas(t, pool, journalPrefix)
	for _, fact := range []string{"created", "proposed", "approved", "rejected", "withdrawn", "validated", "posted", "reversed"} {
		assert.Positive(t, seen["com.zoikosuite.accounting.journal."+fact], "scenario never emitted journal.%s, so its schema is untested", fact)
	}
}

// A schema file exists for exactly the journal facts this service emits.
func TestJournalSchemaFilesAreTheEmittedFacts(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(journalSchemaDir(), "accounting.journal.*.schema.json"))
	require.NoError(t, err)
	var got []string
	for _, f := range files {
		got = append(got, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "accounting.journal."), ".schema.json"))
	}
	assert.ElementsMatch(t, []string{"created", "proposed", "approved", "rejected", "withdrawn", "validated", "posted", "reversed"}, got)
}

// Event facts are dotted (aggregate.fact); rule-condition facts such as "allocation_ref" are not.
var outEventFact = regexp.MustCompile(`Fact:\s*"([a-z0-9-]+\.[a-z0-9.-]+)"`)

// Every kernel event fact the source emits as a literal has a published schema,
// so a new event cannot ship without a contract. (Facts assembled at runtime are
// not visible here; the kernel integration tests validate what they actually emit.)
func TestEveryKernelEventFactHasAPublishedSchema(t *testing.T) {
	_, self, _, _ := runtime.Caller(0)
	internal := filepath.Join(filepath.Dir(self), "..")
	want := map[string]bool{}
	for _, dir := range []string{"kernel", "store"} {
		files, err := filepath.Glob(filepath.Join(internal, dir, "*.go"))
		require.NoError(t, err)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			require.NoError(t, err)
			for _, m := range outEventFact.FindAllSubmatch(b, -1) {
				want["accounting.kernel."+string(m[1])] = true
			}
		}
	}
	require.NotEmpty(t, want, "found no OutEvent facts; the pattern is stale")
	have := map[string]bool{}
	files, err := filepath.Glob(filepath.Join(journalSchemaDir(), "accounting.kernel.*.schema.json"))
	require.NoError(t, err)
	for _, f := range files {
		have[strings.TrimSuffix(filepath.Base(f), ".schema.json")] = true
	}
	for n := range want {
		assert.True(t, have[n], "kernel fact %s is emitted but has no contracts/events/%s.schema.json", n, n)
	}
}
