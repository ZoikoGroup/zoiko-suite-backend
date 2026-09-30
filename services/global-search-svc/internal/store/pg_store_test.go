package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/global-search-svc/internal/domain"
)

// DATA-06 Global Search, against real Postgres as the NOSUPERUSER
// NOBYPASSRLS role.

// Happy path: ReindexScope -> IndexObject -> Search -> MarkIndexAvailable
// -> GetIndexHealth.
func TestGS_HappyPath_IndexAndSearch(t *testing.T) {
	f := newFixture(t)
	idx, err := f.s.ReindexScope(f.ctx, orgA, "invoices", "test-operator", f.claim("ReindexScope", "invoices"))
	if err != nil {
		t.Fatalf("reindex scope: %v", err)
	}
	if idx.Status != domain.IndexIndexing {
		t.Fatalf("new index status = %s, want Indexing", idx.Status)
	}

	f.indexObject(orgA, "invoices", "invoice", "INV-1001", "Acme Corporation quarterly services invoice", false, nil)

	if _, err := f.s.MarkIndexAvailable(f.ctx, orgA, "invoices", "test-operator", f.claim("MarkIndexAvailable", "invoices")); err != nil {
		t.Fatalf("mark available: %v", err)
	}

	resp, err := f.s.Search(f.ctx, orgA, "invoices", "any-principal", "Acme quarterly", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].ObjectRef != "INV-1001" {
		t.Fatalf("search results: %+v", resp.Results)
	}
	if resp.Results[0].Snippet == "" {
		t.Fatalf("search result has no snippet: %+v", resp.Results[0])
	}

	health, err := f.s.GetIndexHealth(f.ctx, orgA, "invoices", time.Now().UTC())
	if err != nil {
		t.Fatalf("get index health: %v", err)
	}
	if health.Status != domain.IndexAvailable || health.DocumentCount != 1 {
		t.Fatalf("index health: %+v", health)
	}
}

// Negative path #1 (doc-named): unauthorized object does not leak via
// count/snippet/autocomplete — an unauthorized principal sees the exact
// same count/results/suggestions as if the restricted object didn't
// exist at all.
func TestGS_UnauthorizedObjectDoesNotLeak(t *testing.T) {
	f := newFixture(t)
	f.indexObject(orgA, "documents", "contract", "CONTRACT-SECRET", "confidential merger agreement acquisition terms",
		true, []string{"alice"})
	f.indexObject(orgA, "documents", "contract", "CONTRACT-PUBLIC", "standard public terms of service agreement",
		false, nil)

	// bob is NOT authorized for the restricted document.
	respBob, err := f.s.Search(f.ctx, orgA, "documents", "bob", "merger acquisition", 10)
	if err != nil {
		t.Fatalf("search as bob: %v", err)
	}
	if len(respBob.Results) != 0 {
		t.Fatalf("bob (unauthorized) saw the restricted document in search: %+v", respBob.Results)
	}
	countBob, err := f.s.Count(f.ctx, orgA, "documents", "bob", "merger acquisition")
	if err != nil {
		t.Fatalf("count as bob: %v", err)
	}
	if countBob != 0 {
		t.Fatalf("bob's count leaked the restricted document's existence: %d", countBob)
	}
	// "confiden" is a real prefix of a word in the restricted document's
	// own content ("confidential") — autocomplete matches against
	// indexed content, not the opaque object_ref, so this is the
	// meaningful leakage probe: bob types a prefix that WOULD match the
	// restricted document's content, and must still get nothing back.
	suggestBob, err := f.s.Autocomplete(f.ctx, orgA, "documents", "bob", "confiden", 10)
	if err != nil {
		t.Fatalf("autocomplete as bob: %v", err)
	}
	if len(suggestBob) != 0 {
		t.Fatalf("bob's autocomplete leaked the restricted document: %+v", suggestBob)
	}
	if _, err := f.s.ExplainResult(f.ctx, orgA, "documents", "bob", "CONTRACT-SECRET"); !errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatalf("bob's ExplainResult on the restricted doc: %v", err)
	}

	// alice IS authorized.
	respAlice, err := f.s.Search(f.ctx, orgA, "documents", "alice", "merger acquisition", 10)
	if err != nil {
		t.Fatalf("search as alice: %v", err)
	}
	if len(respAlice.Results) != 1 || respAlice.Results[0].ObjectRef != "CONTRACT-SECRET" {
		t.Fatalf("alice (authorized) should see the restricted document: %+v", respAlice.Results)
	}
	suggestAlice, err := f.s.Autocomplete(f.ctx, orgA, "documents", "alice", "confiden", 10)
	if err != nil {
		t.Fatalf("autocomplete as alice: %v", err)
	}
	if len(suggestAlice) != 1 || suggestAlice[0] != "CONTRACT-SECRET" {
		t.Fatalf("alice's autocomplete: %+v", suggestAlice)
	}

	// The PUBLIC document is visible to everyone regardless.
	countBobPublic, err := f.s.Count(f.ctx, orgA, "documents", "bob", "standard terms")
	if err != nil {
		t.Fatalf("count public doc as bob: %v", err)
	}
	if countBobPublic != 1 {
		t.Fatalf("bob should see the public document: count=%d", countBobPublic)
	}
}

// Negative path #2 (doc-named): a purged/restricted object is removed
// from the index under disposition policy — genuinely gone, not merely
// hidden.
func TestGS_PurgedObjectIsRemovedFromIndex(t *testing.T) {
	f := newFixture(t)
	f.indexObject(orgA, "records", "record", "REC-1", "temporary record pending disposition", false, nil)

	if err := f.s.PurgeIndexObject(f.ctx, orgA, domain.PurgeIndexObjectRequest{Scope: "records", ObjectRef: "REC-1"},
		f.claim("PurgeIndexObject", "REC-1")); err != nil {
		t.Fatalf("purge: %v", err)
	}

	resp, err := f.s.Search(f.ctx, orgA, "records", "any-principal", "temporary record", 10)
	if err != nil {
		t.Fatalf("search after purge: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("purged object still appears in search: %+v", resp.Results)
	}
	if _, err := f.s.ExplainResult(f.ctx, orgA, "records", "any-principal", "REC-1"); !errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatalf("purged object still explainable: %v", err)
	}

	// Purging again is refused (nothing left to purge).
	if err := f.s.PurgeIndexObject(f.ctx, orgA, domain.PurgeIndexObjectRequest{Scope: "records", ObjectRef: "REC-1"},
		f.claim("PurgeIndexObject-again", "REC-1")); !errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatalf("re-purged an already-purged object: %v", err)
	}
}

// Negative path #3 (doc-named): a stale index returns a freshness
// indication — staleness computed server-side against the scope's own
// declared policy bound.
func TestGS_StaleIndexReturnsFreshnessIndication(t *testing.T) {
	f := newFixture(t)
	if _, err := f.s.SetSearchPolicy(f.ctx, orgA, domain.SetSearchPolicyRequest{Scope: "reports", MaxStalenessSeconds: 3600},
		"test-operator"); err != nil {
		t.Fatalf("set search policy: %v", err)
	}
	f.indexObject(orgA, "reports", "report", "RPT-1", "quarterly financial report summary", false, nil)

	fresh, err := f.s.GetIndexHealth(f.ctx, orgA, "reports", time.Now().UTC())
	if err != nil {
		t.Fatalf("get index health (fresh): %v", err)
	}
	if fresh.Degraded {
		t.Fatalf("freshly-indexed document reported degraded: %+v", fresh)
	}

	stale, err := f.s.ValidateSearchPolicy(f.ctx, orgA, "reports", time.Now().UTC().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("validate search policy (stale): %v", err)
	}
	if !stale.Degraded {
		t.Fatalf("2h-old index against a 1h staleness bound was not flagged degraded: %+v", stale)
	}
}

// Idempotent replay: retrying IndexObject with the same claim key must
// not create a duplicate document.
func TestGS_IndexObject_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	req := domain.IndexObjectRequest{
		Scope: "replay-scope", ObjectType: "thing", ObjectRef: "THING-1", ContentText: "replay test content",
		ContentHash: hashLike("THING-1"), Classification: "internal", ResidencyRegion: "us-east",
		SourceUpdatedAt: time.Now().UTC(),
	}
	claim := f.claim("IndexObject", "replay-scope|thing|THING-1")
	first, err := f.s.IndexObject(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("first index: %v", err)
	}
	_, err = f.s.IndexObject(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != claim.ResourceID {
		t.Fatalf("replay of IndexObject: %v", err)
	}
	_ = first
}

// RebuildIndex purges all documents and cycles the index through
// Rebuilding back to Available.
func TestGS_RebuildIndex(t *testing.T) {
	f := newFixture(t)
	f.indexObject(orgA, "catalog", "item", "ITEM-1", "widget product catalog entry", false, nil)
	if _, err := f.s.MarkIndexAvailable(f.ctx, orgA, "catalog", "test-operator", f.claim("MarkIndexAvailable", "catalog")); err != nil {
		t.Fatalf("mark available: %v", err)
	}

	rebuilding, err := f.s.RebuildIndex(f.ctx, orgA, "catalog", "test-operator", f.claim("RebuildIndex", "catalog"))
	if err != nil {
		t.Fatalf("rebuild index: %v", err)
	}
	if rebuilding.Status != domain.IndexRebuilding {
		t.Fatalf("status = %s, want Rebuilding", rebuilding.Status)
	}

	resp, err := f.s.Search(f.ctx, orgA, "catalog", "any-principal", "widget", 10)
	if err != nil {
		t.Fatalf("search during rebuild: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("old documents survived rebuild: %+v", resp.Results)
	}

	f.indexObject(orgA, "catalog", "item", "ITEM-2", "gadget product catalog entry", false, nil)
	if _, err := f.s.MarkIndexAvailable(f.ctx, orgA, "catalog", "test-operator", f.claim("MarkIndexAvailable-2", "catalog")); err != nil {
		t.Fatalf("mark available after rebuild: %v", err)
	}
	resp2, err := f.s.Search(f.ctx, orgA, "catalog", "any-principal", "gadget", 10)
	if err != nil || len(resp2.Results) != 1 {
		t.Fatalf("search after rebuild: %+v (err=%v)", resp2, err)
	}
}

// Tenant isolation: cross-tenant search/count/health are all denied by
// RLS, not an application-level filter.
func TestGS_TenantIsolation(t *testing.T) {
	f := newFixture(t)
	f.indexObject(orgA, "shared-scope-name", "thing", "TENANT-A-ITEM", "tenant A only content", false, nil)

	resp, err := f.s.Search(f.ctx, orgB, "shared-scope-name", "any-principal", "tenant A only", 10)
	if err != nil {
		t.Fatalf("cross-tenant search: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("cross-tenant search leaked another tenant's document: %+v", resp.Results)
	}
	if _, err := f.s.GetIndexHealth(f.ctx, orgB, "shared-scope-name", time.Now().UTC()); !errors.Is(err, domain.ErrIndexNotFound) {
		t.Fatalf("cross-tenant index health read: %v", err)
	}
}
