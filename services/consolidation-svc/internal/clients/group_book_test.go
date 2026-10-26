package clients_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/consolidation-svc/internal/clients"
	"zoiko.io/consolidation-svc/internal/domain"
)

type glCalls struct {
	mu    sync.Mutex
	calls []string
	who   map[string]string
	body  map[string]any
}

func (g *glCalls) record(r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, r.Method+" "+r.URL.Path)
	g.who[r.URL.Path] = r.Header.Get("X-Principal-Id")
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adjustments") {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &g.body)
	}
}

// groupBook serves the group-book API of general-ledger-svc: groups, a proposal answered with createStatus, the approval and the withdrawal.
func groupBook(t *testing.T, groups string, createStatus int, adjStatus string) (*httptest.Server, *glCalls) {
	g := &glCalls{who: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.record(r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/consolidation-groups":
			_, _ = w.Write([]byte(groups))
		case strings.HasSuffix(r.URL.Path, "/adjustments"):
			w.WriteHeader(createStatus)
			_, _ = w.Write([]byte(`{"adjustment_id":"gl-adj-1","status":"` + adjStatus + `"}`))
		case strings.HasSuffix(r.URL.Path, "/approve"):
			_, _ = w.Write([]byte(`{"adjustment_id":"gl-adj-1","status":"APPROVED"}`))
		case strings.HasSuffix(r.URL.Path, "/withdraw"):
			if strings.Contains(r.URL.Path, "applied") {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"code":"ADJUSTMENT_APPLIED"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"WITHDRAWN"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, g
}

var oneGroup = `{"groups":[{"group_id":"grp-1","parent_entity_id":"le-group"}],"total":1}`

var adjLines = []clients.JournalLine{{AccountCode: "6000", DebitAmount: 40}, {AccountCode: "1000", CreditAmount: 40}}

// A post goes to the group book as a group adjustment, never to an entity journal, is approved there by the principal who approved it
// here, and returns the group adjustment id.
func TestPostConsolidationAdjustmentJournal_SubmitsAGroupAdjustmentApprovedByTheApprover(t *testing.T) {
	srv, g := groupBook(t, oneGroup, http.StatusCreated, "PROPOSED")
	c := clients.New(srv.URL, "", zap.NewNop())
	id, err := c.PostConsolidationAdjustmentJournal(context.Background(), "tenant-1", "poster-1", "approver-1", "le-group", "2026-07", "top-side", "adj-77", "corr", adjLines)
	if err != nil || id != "gl-adj-1" {
		t.Fatalf("%q %v", id, err)
	}
	want := []string{"GET /v1/consolidation-groups", "POST /v1/consolidation-groups/grp-1/adjustments", "POST /v1/consolidation-adjustments/gl-adj-1/approve"}
	if strings.Join(g.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("%v", g.calls)
	}
	for _, call := range g.calls {
		if strings.Contains(call, "/v1/postings/") {
			t.Fatalf("an entity ledger posting path was used: %s", call)
		}
	}
	if g.who["/v1/consolidation-groups/grp-1/adjustments"] != "poster-1" || g.who["/v1/consolidation-adjustments/gl-adj-1/approve"] != "approver-1" {
		t.Fatalf("the author and the approver are different principals: %v", g.who)
	}
	if g.body["source_ref"] != "adj-77" || g.body["evidence_ref"] != "adj-77" || g.body["fiscal_period"] != "2026-07" {
		t.Fatalf("%v", g.body)
	}
	ls := g.body["lines"].([]any)
	if ls[0].(map[string]any)["amount"] != "40.00" || ls[1].(map[string]any)["amount"] != "-40.00" {
		t.Fatalf("lines are signed amounts, debit positive: %v", ls)
	}
}

// A retry finds the adjustment already approved and approves nothing again.
func TestPostConsolidationAdjustmentJournal_RetryDoesNotApproveTwice(t *testing.T) {
	srv, g := groupBook(t, oneGroup, http.StatusOK, "APPROVED")
	c := clients.New(srv.URL, "", zap.NewNop())
	id, err := c.PostConsolidationAdjustmentJournal(context.Background(), "tenant-1", "poster-1", "approver-1", "le-group", "2026-07", "d", "adj-77", "corr", adjLines)
	if err != nil || id != "gl-adj-1" || len(g.calls) != 2 {
		t.Fatalf("%q %v %v", id, err, g.calls)
	}
}

// No group of the group book, or an ambiguous one, is a refusal that names the entity, not a post into some default book.
func TestPostConsolidationAdjustmentJournal_NeedsExactlyOneGroup(t *testing.T) {
	for what, groups := range map[string]string{"none": `{"groups":[],"total":0}`, "two": `{"groups":[{"group_id":"a"},{"group_id":"b"}],"total":2}`} {
		srv, g := groupBook(t, groups, http.StatusCreated, "PROPOSED")
		_, err := clients.New(srv.URL, "", zap.NewNop()).PostConsolidationAdjustmentJournal(context.Background(), "tenant-1", "p", "a", "le-group", "2026-07", "d", "adj-1", "c", adjLines)
		if !errors.Is(err, domain.ErrNoGroupBook) || len(g.calls) != 1 {
			t.Fatalf("%s: %v %v", what, err, g.calls)
		}
	}
}

func TestPostConsolidationAdjustmentJournal_GLRefusalIsAnError(t *testing.T) {
	srv, _ := groupBook(t, oneGroup, http.StatusUnprocessableEntity, "")
	_, err := clients.New(srv.URL, "", zap.NewNop()).PostConsolidationAdjustmentJournal(context.Background(), "tenant-1", "p", "a", "le-group", "2026-07", "d", "adj-1", "c", adjLines)
	if !errors.Is(err, domain.ErrGLServiceUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestReverseConsolidationAdjustmentJournal_WithdrawsTheGroupAdjustment(t *testing.T) {
	srv, g := groupBook(t, oneGroup, http.StatusCreated, "PROPOSED")
	c := clients.New(srv.URL, "", zap.NewNop())
	if err := c.ReverseConsolidationAdjustmentJournal(context.Background(), "tenant-1", "rev-1", "le-group", "gl-adj-1", "wrong"); err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 1 || g.calls[0] != "POST /v1/consolidation-adjustments/gl-adj-1/withdraw" {
		t.Fatalf("%v", g.calls)
	}
	// one a consolidation run has applied is refused, and says why
	err := c.ReverseConsolidationAdjustmentJournal(context.Background(), "tenant-1", "rev-1", "le-group", "applied", "wrong")
	if !errors.Is(err, domain.ErrAdjustmentInGroupRun) {
		t.Fatalf("%v", err)
	}
}
