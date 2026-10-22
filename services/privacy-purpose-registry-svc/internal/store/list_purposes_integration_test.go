package store_test

import (
	"testing"
	"time"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/store"
)

// Real-PostgreSQL proof that ListPurposes returns the CURRENTLY PUBLISHED
// purposes: one row per purpose, the published version in force now — the same
// version ResolvePurposeAsOf(now, now) returns — under RLS as a restricted role.

func listed(t *testing.T, st *store.PgStore, scope string) map[string]domain.PurposeVersion {
	t.Helper()
	out := map[string]domain.PurposeVersion{}
	for _, v := range mustT[[]domain.PurposeVersion](t, "ListPurposes")(st.ListPurposes(tctx(scope))) {
		if prev, dup := out[v.PurposeID]; dup {
			t.Fatalf("purpose %s listed twice (%s, %s)", v.PurposeID, prev.PurposeVersionID, v.PurposeVersionID)
		}
		out[v.PurposeID] = v
	}
	return out
}

// publishSuccessor creates a successor draft of parent (optionally dated) and publishes it.
func publishSuccessor(t *testing.T, st *store.PgStore, scope string, parent *domain.PurposeVersion, effective *time.Time) *domain.PurposeVersion {
	t.Helper()
	ctx := tctx(scope)
	d := mustT[*domain.PurposeVersion](t, "CreatePurposeVersion")(st.CreatePurposeVersion(ctx, parent.PurposeID, domain.CreatePurposeVersionRequest{
		ParentVersionID: parent.PurposeVersionID, Statement: "Amended", CompatibilityClass: "PRIMARY",
		LawfulBasisRefs: []string{"basis-contract"}, NecessityRationale: "still needed", EffectiveFrom: effective,
	}, "user-1"))
	return mustT[*domain.PurposeVersion](t, "PublishPurposeVersion")(st.PublishPurposeVersion(ctx, d.PurposeID, d.PurposeVersionID, "user-2", "corr-pub"))
}

func TestListPurposes_InForceVersionOnly(t *testing.T) {
	st, _ := newStore(t)
	if st == nil {
		return
	}

	// 1. one published version -> returned.
	v1 := mkPurpose(t, st, tenantA)
	if got := listed(t, st, tenantA); len(got) != 1 || got[v1.PurposeID].PurposeVersionID != v1.PurposeVersionID {
		t.Fatalf("a single published version must be listed, got %+v", got)
	}

	// 2 + 4. a later published version supersedes it; only the current one of the purpose is returned.
	time.Sleep(5 * time.Millisecond)
	v2 := publishSuccessor(t, st, tenantA, v1, nil)
	got := listed(t, st, tenantA)
	if len(got) != 1 || got[v1.PurposeID].PurposeVersionID != v2.PurposeVersionID {
		t.Fatalf("only the current version %s may be listed (not superseded %s), got %+v", v2.PurposeVersionID, v1.PurposeVersionID, got)
	}

	// 3. a future-dated published version is not in force: v2 stays current.
	future := time.Now().UTC().Add(time.Hour)
	v3 := publishSuccessor(t, st, tenantA, v2, &future)
	if !v3.EffectiveFrom.After(time.Now()) {
		t.Fatalf("precondition: v3 must stay future-dated, effective_from=%s", v3.EffectiveFrom)
	}
	got = listed(t, st, tenantA)
	if got[v1.PurposeID].PurposeVersionID != v2.PurposeVersionID {
		t.Fatalf("future-dated v3 must not be listed yet; got %s want %s", got[v1.PurposeID].PurposeVersionID, v2.PurposeVersionID)
	}

	// The list agrees with GET /privacy/purposes/{id} (ResolvePurposeAsOf at the current instant).
	now := time.Now().UTC()
	resolved := mustT[*domain.PurposeVersion](t, "ResolvePurposeAsOf")(st.ResolvePurposeAsOf(tctx(tenantA), v1.PurposeID, now, now))
	if resolved.PurposeVersionID != got[v1.PurposeID].PurposeVersionID {
		t.Fatalf("list (%s) and resolve (%s) disagree on the in-force version", got[v1.PurposeID].PurposeVersionID, resolved.PurposeVersionID)
	}

	// An unpublished draft and a never-published purpose are not listed.
	ctx := tctx(tenantA)
	_ = mustT[*domain.PurposeVersion](t, "draft successor")(st.CreatePurposeVersion(ctx, v1.PurposeID, domain.CreatePurposeVersionRequest{
		ParentVersionID: v2.PurposeVersionID, Statement: "draft", CompatibilityClass: "PRIMARY",
	}, "user-1"))
	if _, _, err := st.CreatePurpose(ctx, recordTenant(tenantA), domain.CreatePurposeRequest{Statement: "never published", CompatibilityClass: "PRIMARY"}, "user-1"); err != nil {
		t.Fatalf("CreatePurpose: %v", err)
	}
	got = listed(t, st, tenantA)
	if got[v1.PurposeID].PurposeVersionID != v2.PurposeVersionID {
		t.Fatalf("a draft successor must not displace the published version")
	}
	if len(got) != 1 {
		t.Fatalf("a never-published purpose must not be listed, got %d rows", len(got))
	}
}

// A version dated a moment ahead enters force when its time arrives.
func TestListPurposes_FutureVersionEntersForceWhenItsTimeArrives(t *testing.T) {
	st, _ := newStore(t)
	if st == nil {
		return
	}
	v1 := mkPurpose(t, st, tenantA)
	soon := time.Now().UTC().Add(1500 * time.Millisecond)
	v2 := publishSuccessor(t, st, tenantA, v1, &soon)
	if got := listed(t, st, tenantA)[v1.PurposeID]; got.PurposeVersionID != v1.PurposeVersionID {
		t.Fatalf("before its time v2 must not be listed, got %s", got.PurposeVersionID)
	}
	time.Sleep(2 * time.Second)
	if got := listed(t, st, tenantA)[v1.PurposeID]; got.PurposeVersionID != v2.PurposeVersionID {
		t.Fatalf("once in force v2 must replace v1, got %s", got.PurposeVersionID)
	}
}

func TestListPurposes_TenantIsolationAndPlatformBaseline(t *testing.T) {
	st, _ := newStore(t)
	if st == nil {
		return
	}
	a := mkPurpose(t, st, tenantA)
	b := mkPurpose(t, st, tenantB)
	plat := mkPurpose(t, st, platform)

	gotA, gotB, gotP := listed(t, st, tenantA), listed(t, st, tenantB), listed(t, st, platform)
	if _, ok := gotA[a.PurposeID]; !ok {
		t.Errorf("tenant A must list its own purpose")
	}
	if _, ok := gotA[b.PurposeID]; ok {
		t.Errorf("tenant A listed tenant B's purpose")
	}
	if _, ok := gotB[a.PurposeID]; ok {
		t.Errorf("tenant B listed tenant A's purpose")
	}
	if _, ok := gotP[a.PurposeID]; ok || func() bool { _, k := gotP[b.PurposeID]; return k }() {
		t.Errorf("the platform plane listed a tenant's purpose")
	}
	for name, got := range map[string]map[string]domain.PurposeVersion{"A": gotA, "B": gotB, "platform": gotP} {
		if _, ok := got[plat.PurposeID]; !ok {
			t.Errorf("%s must list the platform baseline purpose", name)
		}
	}
}
