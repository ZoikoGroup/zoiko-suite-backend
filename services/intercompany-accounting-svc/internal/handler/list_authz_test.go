package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
)

// entityPolicy is a policy-driven authz stub: the principal may view exactly
// the entities in allow; every other entity is denied. unavailable, when set,
// is returned for every check (authorization-svc unreachable).
type entityPolicy struct {
	allow       map[string]bool
	unavailable error
	checked     []string
}

func (p *entityPolicy) CheckAllowed(_ context.Context, _, legalEntityID, _ string) error {
	p.checked = append(p.checked, legalEntityID)
	if p.unavailable != nil {
		return p.unavailable
	}
	if p.allow[legalEntityID] {
		return nil
	}
	return domain.ErrAuthorizationDenied
}

func seedPairs(s *stubStoreReal) {
	for id, pair := range map[string][2]string{
		"pair-a-b": {"entity-a", "entity-b"},
		"pair-c-b": {"entity-c", "entity-b"},
		"pair-c-a": {"entity-c", "entity-a"},
	} {
		s.entries[id] = &domain.IntercompanyEntry{IntercompanyEntryID: id, SourceLegalEntityID: pair[0], TargetLegalEntityID: pair[1]}
	}
}

func listIDs(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var got []domain.IntercompanyEntry
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode list: %v (%s)", err, body)
	}
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.IntercompanyEntryID] = true
	}
	return ids
}

// Regression: with no entity filter the list skipped authorization and
// returned every pair in the tenant, including entity-c's, which this
// principal has no authority over.
func TestListEntries_NoFilterReturnsOnlyAuthorizedSources(t *testing.T) {
	s := newStubStoreReal()
	seedPairs(s)
	r := newRouter(s, &entityPolicy{allow: map[string]bool{"entity-a": true}}, &stubLedger{})

	rr := doReq(r, http.MethodGet, "/v1/intercompany/entries", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	ids := listIDs(t, rr.Body.Bytes())
	if !ids["pair-a-b"] || ids["pair-c-b"] || ids["pair-c-a"] || len(ids) != 1 {
		t.Fatalf("visible pairs = %v, want only pair-a-b", ids)
	}
}

// Filtering on a target the caller may view must not reveal pairs whose
// source the caller may not view (GetEntry authorizes on the source).
func TestListEntries_TargetFilterStillHidesUnauthorizedSources(t *testing.T) {
	s := newStubStoreReal()
	seedPairs(s)
	r := newRouter(s, &entityPolicy{allow: map[string]bool{"entity-a": true, "entity-b": true}}, &stubLedger{})

	rr := doReq(r, http.MethodGet, "/v1/intercompany/entries?target_legal_entity_id=entity-b", nil, "principal-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	ids := listIDs(t, rr.Body.Bytes())
	if !ids["pair-a-b"] || ids["pair-c-b"] {
		t.Fatalf("visible pairs = %v, want pair-a-b only", ids)
	}
}

// Both filters are authorized, not only the first one present.
func TestListEntries_DeniedTargetFilterIs403(t *testing.T) {
	s := newStubStoreReal()
	seedPairs(s)
	r := newRouter(s, &entityPolicy{allow: map[string]bool{"entity-a": true}}, &stubLedger{})

	rr := doReq(r, http.MethodGet, "/v1/intercompany/entries?source_legal_entity_id=entity-a&target_legal_entity_id=entity-b", nil, "principal-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rr.Code, rr.Body.String())
	}
}

// An unreachable authorization-svc fails the list closed, never unfiltered.
func TestListEntries_AuthzUnavailableFailsClosed(t *testing.T) {
	s := newStubStoreReal()
	seedPairs(s)
	r := newRouter(s, &entityPolicy{unavailable: errors.New("authorization-svc unreachable")}, &stubLedger{})

	rr := doReq(r, http.MethodGet, "/v1/intercompany/entries", nil, "principal-1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rr.Code, rr.Body.String())
	}
}

func TestListEntries_MissingPrincipalIsRejected(t *testing.T) {
	s := newStubStoreReal()
	seedPairs(s)
	r := newRouter(s, &entityPolicy{allow: map[string]bool{"entity-a": true}}, &stubLedger{})

	rr := doReq(r, http.MethodGet, "/v1/intercompany/entries", nil, "")
	if rr.Code == http.StatusOK {
		t.Fatalf("a request with no principal must not list entries; got 200 %s", rr.Body.String())
	}
}
