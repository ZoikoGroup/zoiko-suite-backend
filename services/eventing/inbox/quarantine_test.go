package inbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func quarantineOne(t *testing.T, in *Inbox, id string) {
	t.Helper()
	if _, err := in.Process(context.Background(), msg(id), func(context.Context, pgx.Tx, Receipt) (string, error) {
		return "", Poison(errors.New("bad data"))
	}); err == nil {
		t.Fatal("expected poison failure")
	}
}

func goodOpen() CaseOpen {
	return CaseOpen{Owner: "ar-team", Priority: "high", FailureClass: ClassPermanentData, SLA: 4 * time.Hour}
}

func TestOpenCaseCopiesReceiptFacts(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()
	quarantineOne(t, in, "evt-q1")
	if n, _ := in.UnownedQuarantined(ctx, "tenant-a"); n != 1 {
		t.Fatalf("unowned = %d", n)
	}
	c, err := in.OpenCase(ctx, "tenant-a", "evt-q1", goodOpen())
	if err != nil {
		t.Fatal(err)
	}
	if c.Owner != "ar-team" || c.ErrorCode != "POISON_MESSAGE" || c.PayloadHash != "sha256:aaa" || c.EventType != msg("x").EventType {
		t.Fatalf("case does not mirror the receipt: %+v", c)
	}
	if c.Overdue(time.Now()) || !c.Overdue(time.Now().Add(5*time.Hour)) {
		t.Fatal("SLA arithmetic wrong")
	}
	if n, _ := in.UnownedQuarantined(ctx, "tenant-a"); n != 0 {
		t.Fatalf("a case must remove it from the unowned count, got %d", n)
	}
	if _, err := in.OpenCase(ctx, "tenant-a", "evt-q1", goodOpen()); !errors.Is(err, ErrCaseExists) {
		t.Fatalf("second case: %v", err)
	}
}

func TestOpenCaseRefusals(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()
	if _, err := in.Process(ctx, msg("evt-ok"), handler); err != nil {
		t.Fatal(err)
	}
	if _, err := in.OpenCase(ctx, "tenant-a", "evt-ok", goodOpen()); !errors.Is(err, ErrNotQuarantined) {
		t.Fatalf("completed receipt: %v", err)
	}
	if _, err := in.OpenCase(ctx, "tenant-a", "missing", goodOpen()); !errors.Is(err, ErrNotQuarantined) {
		t.Fatalf("missing receipt: %v", err)
	}
	for name, o := range map[string]CaseOpen{
		"no owner":  {Priority: "high", FailureClass: ClassPoison, SLA: time.Hour},
		"bad class": {Owner: "x", Priority: "high", FailureClass: "weird", SLA: time.Hour},
		"bad prio":  {Owner: "x", Priority: "urgent", FailureClass: ClassPoison, SLA: time.Hour},
		"no sla":    {Owner: "x", Priority: "high", FailureClass: ClassPoison},
	} {
		if _, err := in.OpenCase(ctx, "tenant-a", "evt-ok", o); !errors.Is(err, ErrInvalidCase) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestResolveCaseNeedsDispositionResolverAndEvidence(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()
	quarantineOne(t, in, "evt-q2")
	if _, err := in.OpenCase(ctx, "tenant-a", "evt-q2", goodOpen()); err != nil {
		t.Fatal(err)
	}
	if err := in.ResolveCase(ctx, "tenant-a", "evt-q2", "shrug", "alice", []string{"CHG-1"}); !errors.Is(err, ErrInvalidCase) {
		t.Fatalf("bad disposition: %v", err)
	}
	if err := in.ResolveCase(ctx, "tenant-a", "evt-q2", DispAcceptedException, "", []string{"CHG-1"}); !errors.Is(err, ErrInvalidCase) {
		t.Fatalf("no resolver: %v", err)
	}
	if err := in.ResolveCase(ctx, "tenant-a", "evt-q2", DispAcceptedException, "alice", nil); !errors.Is(err, ErrEvidenceRequired) {
		t.Fatalf("no evidence: %v", err)
	}
	if err := in.ResolveCase(ctx, "tenant-a", "evt-q2", DispAcceptedException, "alice", []string{"CHG-1"}); err != nil {
		t.Fatal(err)
	}
	c, _ := in.GetCase(ctx, "tenant-a", "evt-q2")
	if c.Disposition != DispAcceptedException || c.ResolvedBy != "alice" || c.ResolvedAt == nil || len(c.EvidenceRefs) != 1 {
		t.Fatalf("resolution not recorded: %+v", c)
	}
	if err := in.ResolveCase(ctx, "tenant-a", "evt-q2", DispFixAndReplay, "bob", []string{"CHG-2"}); !errors.Is(err, ErrCaseResolved) {
		t.Fatalf("re-resolve: %v", err)
	}
	if err := in.ResolveCase(ctx, "tenant-a", "nope", DispFixAndReplay, "bob", []string{"x"}); !errors.Is(err, ErrCaseNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if open, _ := in.ListOpenCases(ctx, "tenant-a", 10); len(open) != 0 {
		t.Fatalf("resolved case still listed open: %d", len(open))
	}
}

// The database, not only the Go code, refuses a half-resolved case.
func TestCaseTableRefusesHalfResolution(t *testing.T) {
	h := newHarness(t)
	in := h.inbox(t, 3)
	ctx := context.Background()
	quarantineOne(t, in, "evt-q3")
	if _, err := in.OpenCase(ctx, "tenant-a", "evt-q3", goodOpen()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `UPDATE eventing_quarantine_case SET disposition = 'fix-and-replay' WHERE event_id = 'evt-q3'`); err == nil {
		t.Fatal("a disposition without resolver, time and evidence must violate the table constraint")
	}
}
