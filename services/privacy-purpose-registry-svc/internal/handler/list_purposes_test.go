package handler_test

import (
	"net/http"
	"testing"
	"time"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

// GET /privacy/purposes lists the CURRENTLY PUBLISHED purposes: one row per
// purpose — the published version in force now. The authoritative resolution
// rule is the one GET /privacy/purposes/{id} already applies (published,
// effective_from reached, latest effective_from wins).

func listIDs(t *testing.T, r http.Handler, scope string) map[string]domain.PurposeVersion {
	t.Helper()
	w := call(r, http.MethodGet, purposesPath, nil, scope, "reader-01")
	wantOK(t, w, http.StatusOK)
	out := map[string]domain.PurposeVersion{}
	for _, v := range decode[[]domain.PurposeVersion](t, w) {
		if prev, dup := out[v.PurposeID]; dup {
			t.Fatalf("purpose %s listed twice: %s and %s", v.PurposeID, prev.PurposeVersionID, v.PurposeVersionID)
		}
		out[v.PurposeID] = v
	}
	return out
}

// successor creates a successor draft of parent (optionally future-dated) and,
// when publish is set, publishes it as the checker.
func successor(t *testing.T, r http.Handler, scope string, parent *domain.PurposeVersion, effective *time.Time, publish bool) domain.PurposeVersion {
	t.Helper()
	w := call(r, http.MethodPost, purposesPath+"/"+parent.PurposeID+"/versions", domain.CreatePurposeVersionRequest{
		ParentVersionID: parent.PurposeVersionID, Statement: "Amended statement", CompatibilityClass: "PRIMARY",
		LawfulBasisRefs: []string{"basis-contract-performance"}, NecessityRationale: "still only the email address",
		EffectiveFrom: effective,
	}, scope, maker)
	wantOK(t, w, http.StatusCreated)
	v := decode[domain.PurposeVersion](t, w)
	if !publish {
		return v
	}
	w = call(r, http.MethodPost, purposesPath+"/"+v.PurposeID+"/versions/"+v.PurposeVersionID+"/publish", nil, scope, checker)
	wantOK(t, w, http.StatusOK)
	return decode[domain.PurposeVersion](t, w)
}

func TestListPurposes_CurrentlyPublishedVersionIsReturned(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	got := listIDs(t, r, tenantA)
	if len(got) != 1 || got[p.PurposeID].PurposeVersionID != p.PurposeVersionID {
		t.Fatalf("want exactly the one published version %s, got %+v", p.PurposeVersionID, got)
	}
}

func TestListPurposes_SupersededVersionIsNotReturned_OnlyTheCurrentOneOfEachPurpose(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	v1 := newPurpose(t, r, tenantA)
	time.Sleep(5 * time.Millisecond) // v2 must publish strictly later than v1
	v2 := successor(t, r, tenantA, v1, nil, true)
	other := newPurpose(t, r, tenantA)

	got := listIDs(t, r, tenantA)
	if len(got) != 2 {
		t.Fatalf("want one row per purpose (2), got %d: %+v", len(got), got)
	}
	if got[v1.PurposeID].PurposeVersionID != v2.PurposeVersionID {
		t.Fatalf("purpose %s must list its current version %s, not the superseded %s; got %s",
			v1.PurposeID, v2.PurposeVersionID, v1.PurposeVersionID, got[v1.PurposeID].PurposeVersionID)
	}
	if got[other.PurposeID].PurposeVersionID != other.PurposeVersionID {
		t.Fatalf("the other purpose must still be listed")
	}
}

func TestListPurposes_FutureDatedPublishedVersionIsNotReturned(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	v1 := newPurpose(t, r, tenantA)
	future := time.Now().UTC().Add(time.Hour)
	v2 := successor(t, r, tenantA, v1, &future, true)
	if !v2.EffectiveFrom.After(time.Now()) {
		t.Fatalf("precondition: v2 must be future-dated, effective_from=%s", v2.EffectiveFrom)
	}
	got := listIDs(t, r, tenantA)
	if got[v1.PurposeID].PurposeVersionID != v1.PurposeVersionID {
		t.Fatalf("the future-dated v2 must not replace v1 until it is in force; got %s", got[v1.PurposeID].PurposeVersionID)
	}

	// A purpose whose ONLY published version is future-dated is not in force at all.
	w := call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{
		Statement: "Future purpose", CompatibilityClass: "PRIMARY", LawfulBasisRefs: []string{"b"},
		NecessityRationale: "n", EffectiveFrom: &future,
	}, tenantA, maker)
	wantOK(t, w, http.StatusCreated)
	fut := decode[domain.PurposeVersion](t, w)
	wantOK(t, call(r, http.MethodPost, purposesPath+"/"+fut.PurposeID+"/versions/"+fut.PurposeVersionID+"/publish", nil, tenantA, checker), http.StatusOK)
	if _, listed := listIDs(t, r, tenantA)[fut.PurposeID]; listed {
		t.Fatalf("a purpose that is only future-dated must not be listed")
	}
}

func TestListPurposes_UnpublishedDraftIsNotReturned(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	v1 := newPurpose(t, r, tenantA)
	_ = successor(t, r, tenantA, v1, nil, false) // a DRAFT successor
	w := call(r, http.MethodPost, purposesPath, domain.CreatePurposeRequest{Statement: "Draft only", CompatibilityClass: "PRIMARY"}, tenantA, maker)
	wantOK(t, w, http.StatusCreated)
	draft := decode[domain.PurposeVersion](t, w)

	got := listIDs(t, r, tenantA)
	if got[v1.PurposeID].PurposeVersionID != v1.PurposeVersionID {
		t.Fatalf("a draft successor must not displace the published version")
	}
	if _, listed := got[draft.PurposeID]; listed {
		t.Fatalf("a never-published purpose must not be listed")
	}
}

func TestListPurposes_TenantIsolationAndPlatformBaseline(t *testing.T) {
	r := newRouter(newStubStore(), &stubAuthz{})
	a := newPurpose(t, r, tenantA)
	b := newPurpose(t, r, tenantB)
	plat := newPurpose(t, r, platform)

	gotA := listIDs(t, r, tenantA)
	if _, ok := gotA[a.PurposeID]; !ok {
		t.Fatalf("tenant A must see its own purpose")
	}
	if _, ok := gotA[b.PurposeID]; ok {
		t.Fatalf("tenant A listed tenant B's purpose")
	}
	if _, ok := gotA[plat.PurposeID]; !ok {
		t.Fatalf("platform purposes are a shared baseline readable by every tenant")
	}
	gotB := listIDs(t, r, tenantB)
	if _, ok := gotB[a.PurposeID]; ok {
		t.Fatalf("tenant B listed tenant A's purpose")
	}
	gotP := listIDs(t, r, platform)
	if _, ok := gotP[a.PurposeID]; ok {
		t.Fatalf("the platform plane listed a tenant's purpose")
	}
	if _, ok := gotP[plat.PurposeID]; !ok {
		t.Fatalf("the platform plane must list its own purpose")
	}
}

// The list keeps the same pre-flight as before: principal, tenant, then the
// PRIVACY_PURPOSE_READ grant, failing closed.
func TestListPurposes_PreflightUnchanged(t *testing.T) {
	st, az := newStubStore(), &stubAuthz{}
	r := newRouter(st, az)
	wantErr(t, call(r, http.MethodGet, purposesPath, nil, tenantA, ""), http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed)
	wantErr(t, call(r, http.MethodGet, purposesPath, nil, "", "reader-01"), http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired)
	az.deny = true
	wantErr(t, call(r, http.MethodGet, purposesPath, nil, tenantA, "reader-01"), http.StatusForbidden, domain.ErrCodeAuthorizationDenied)
	az.deny, az.unavailable = false, true
	wantErr(t, call(r, http.MethodGet, purposesPath, nil, tenantA, "reader-01"), http.StatusServiceUnavailable, domain.ErrCodeAuthzUnavailable)
	az.unavailable = false
	az.reset()
	wantOK(t, call(r, http.MethodGet, purposesPath, nil, tenantA, "reader-01"), http.StatusOK)
	if got := az.last(); got.action != "PRIVACY_PURPOSE_READ" || got.scope != tenantA {
		t.Fatalf("authz asked %+v, want PRIVACY_PURPOSE_READ in %s", got, tenantA)
	}
}
