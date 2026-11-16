package store_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/ledger"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

func strp(s string) *string { return &s }

// seedChain records n decisions for tenant-1 with decided_at deliberately
// out of order: the chain follows storage order, never the caller's clock.
func seedChain(t *testing.T, ctx context.Context, s *store.PgStore, n int) []*domain.GovernanceDecision {
	t.Helper()
	var out []*domain.GovernanceDecision
	for i := 0; i < n; i++ {
		d := sampleDecision(fmt.Sprintf("chain-%02d", i+1))
		d.DecidedAt = time.Now().UTC().Add(-time.Duration((i%2)*48+i) * time.Hour).Truncate(time.Microsecond)
		stored, created, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d})
		if err != nil || !created {
			t.Fatalf("record %s: created=%v err=%v", d.DecisionID, created, err)
		}
		out = append(out, stored)
	}
	return out
}

func TestLedger_ChainIsLinkedInStorageOrderAndVerifies(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	stored := seedChain(t, ctx, s, 4)
	other := sampleDecision("other-tenant")
	other.TenantID = "tenant-2"
	if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: other}); err != nil {
		t.Fatal(err)
	}

	for i, d := range stored {
		if d.ChainSeq != int64(i+1) {
			t.Fatalf("%s: chain_seq %d, want %d (storage order)", d.DecisionID, d.ChainSeq, i+1)
		}
		if want := domain.DisplayRef(d.RecordedAt, d.ChainSeq); d.DisplayRef != want || !strings.HasPrefix(d.DisplayRef, "GD-") {
			t.Fatalf("display ref %q, want %q", d.DisplayRef, want)
		}
	}
	if stored[0].PrevHash != hex.EncodeToString(ledger.Genesis) {
		t.Fatalf("the first decision must chain to the genesis hash, got %s", stored[0].PrevHash)
	}
	if stored[1].PrevHash != stored[0].CurrHash {
		t.Fatal("decision 2 must chain to decision 1")
	}
	if stored[0].Domain != "PEOPLE_PAYROLL" {
		t.Fatalf("PAYROLL_RELEASE must classify as PEOPLE_PAYROLL, got %q", stored[0].Domain)
	}

	v, err := s.VerifyChain(ctx, "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid || v.DecisionsChecked != 4 || v.HeadSeq != 4 || v.FirstBreak != nil {
		t.Fatalf("an untouched chain must verify: %+v", v)
	}
	if v2, _ := s.VerifyChain(ctx, "tenant-2"); v2.HeadSeq != 1 {
		t.Fatalf("tenant-2 has its own chain starting at 1, got head %d", v2.HeadSeq)
	}
}

// The outcome of decision 2 is rewritten behind the append-only trigger's back,
// as someone with database superuser access could.
func TestLedger_TamperIsDetectedAndBlocksSealing(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedChain(t, ctx, s, 3)

	if _, err := pool.Exec(ctx, `
		ALTER TABLE governance_decisions DISABLE TRIGGER governance_decisions_immutable;
		UPDATE governance_decisions SET outcome = 'GRANTED' WHERE tenant_id = 'tenant-1' AND chain_seq = 2;
		ALTER TABLE governance_decisions ENABLE TRIGGER governance_decisions_immutable;`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	v, err := s.VerifyChain(ctx, "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Valid || v.FirstBreak == nil || v.FirstBreak.ChainSeq != 2 || v.FirstBreak.Reason != "HASH_MISMATCH" {
		t.Fatalf("a rewritten outcome must break the chain at 2: %+v", v.FirstBreak)
	}
	ev, err := s.DecisionEvidence(ctx, "tenant-1", "chain-02")
	if err != nil || ev.HashValid {
		t.Fatalf("the decision's own evidence must show its hash invalid: %+v %v", ev, err)
	}
	_, _, err = s.SealPackage(ctx, store.SealRequest{TenantID: "tenant-1", PrincipalID: "p", IdempotencyKey: "k", RequestHash: []byte{1}})
	if !errors.Is(err, domain.ErrChainBroken) {
		t.Fatalf("sealing a broken chain must be refused, got %v", err)
	}
}

func TestLedger_SealVerifyAndDetectAFullRewrite(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	stored := seedChain(t, ctx, s, 3)

	pkg, created, err := s.SealPackage(ctx, store.SealRequest{TenantID: "tenant-1", PrincipalID: "sealer", Label: strp("Nov"),
		IdempotencyKey: "seal-1", RequestHash: []byte{1}})
	if err != nil || !created {
		t.Fatalf("seal: %v %v", created, err)
	}
	var leaves [][]byte
	for _, d := range stored {
		b, _ := hex.DecodeString(d.CurrHash)
		leaves = append(leaves, b)
	}
	if pkg.ToSeq != 3 || pkg.PackageSeq != 1 || pkg.MerkleRoot != hex.EncodeToString(ledger.MerkleRoot(leaves)) ||
		pkg.HeadHash != stored[2].CurrHash || pkg.PrevPackageHash != hex.EncodeToString(ledger.Genesis) {
		t.Fatalf("package does not commit to the chain: %+v", pkg)
	}

	again, created, err := s.SealPackage(ctx, store.SealRequest{TenantID: "tenant-1", PrincipalID: "sealer", IdempotencyKey: "seal-1", RequestHash: []byte{1}})
	if err != nil || created || again.PackageID != pkg.PackageID {
		t.Fatalf("a retry with the same key must return the first package: %v %v", created, err)
	}
	if _, _, err := s.SealPackage(ctx, store.SealRequest{TenantID: "tenant-1", PrincipalID: "sealer", IdempotencyKey: "seal-2", RequestHash: []byte{2}}); !errors.Is(err, domain.ErrNothingToSeal) {
		t.Fatalf("sealing an already-sealed head must be NOTHING_TO_SEAL, got %v", err)
	}

	seedMore := sampleDecision("chain-after")
	if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: seedMore}); err != nil {
		t.Fatal(err)
	}
	pkg2, created, err := s.SealPackage(ctx, store.SealRequest{TenantID: "tenant-1", PrincipalID: "sealer", IdempotencyKey: "seal-3", RequestHash: []byte{3}})
	if err != nil || !created || pkg2.PackageSeq != 2 || pkg2.ToSeq != 4 || pkg2.PrevPackageHash != pkg.PackageHash {
		t.Fatalf("second package must chain to the first: %+v %v", pkg2, err)
	}

	if c, err := s.VerifyPackage(ctx, "tenant-1", pkg.PackageID); err != nil || !c.Valid {
		t.Fatalf("an untouched package must verify: %+v %v", c, err)
	}
	if list, err := s.ListPackages(ctx, "tenant-1"); err != nil || len(list) != 2 || list[0].PackageSeq != 2 {
		t.Fatalf("packages newest first: %v %v", list, err)
	}

	// A careful attacker rewrites decision 2 AND recomputes every link after
	// it: the chain verifies on its own, the sealed root does not.
	if _, err := pool.Exec(ctx, `
		ALTER TABLE governance_decisions DISABLE TRIGGER governance_decisions_immutable;
		UPDATE governance_decisions SET outcome = 'GRANTED' WHERE tenant_id = 'tenant-1' AND chain_seq = 2;
		DO $$
		DECLARE r governance_decisions; v_hash BYTEA;
		BEGIN
			SELECT curr_hash INTO v_hash FROM governance_decisions WHERE tenant_id = 'tenant-1' AND chain_seq = 1;
			FOR r IN SELECT * FROM governance_decisions WHERE tenant_id = 'tenant-1' AND chain_seq >= 2 ORDER BY chain_seq LOOP
				r.prev_hash := v_hash;
				r.curr_hash := sha256(v_hash || convert_to(gdl_canonical(r), 'UTF8'));
				UPDATE governance_decisions SET prev_hash = r.prev_hash, curr_hash = r.curr_hash
				 WHERE tenant_id = r.tenant_id AND decision_id = r.decision_id;
				v_hash := r.curr_hash;
			END LOOP;
		END $$;
		ALTER TABLE governance_decisions ENABLE TRIGGER governance_decisions_immutable;`); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	v, err := s.VerifyChain(ctx, "tenant-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.FirstBreak != nil {
		t.Fatalf("the rewritten chain is internally consistent, so no link breaks: %+v", v.FirstBreak)
	}
	if v.Valid || len(v.Packages) != 2 || v.Packages[0].Valid || v.Packages[0].Reason != "ROOT_MISMATCH" {
		t.Fatalf("the sealed packages must expose the rewrite: %+v", v.Packages)
	}
	if c, _ := s.VerifyPackage(ctx, "tenant-1", pkg.PackageID); c.Valid {
		t.Fatal("VerifyPackage must report the rewrite")
	}
}

func TestLedger_SealThroughUnknownDecisionIsNotFound(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	if _, _, err := s.SealPackage(ctx, store.SealRequest{TenantID: "tenant-1", PrincipalID: "p", IdempotencyKey: "k", RequestHash: []byte{1}}); !errors.Is(err, domain.ErrNothingToSeal) {
		t.Fatalf("an empty ledger has nothing to seal, got %v", err)
	}
	seedChain(t, ctx, s, 1)
	if _, _, err := s.SealPackage(ctx, store.SealRequest{TenantID: "tenant-1", PrincipalID: "p", ThroughDecisionID: "nope",
		IdempotencyKey: "k2", RequestHash: []byte{1}}); !errors.Is(err, domain.ErrDecisionNotFound) {
		t.Fatalf("expected ErrDecisionNotFound, got %v", err)
	}
}

// The trigger links the chain for the runtime role too (no superuser, RLS
// forced): the advisory lock and the head read work under row security.
func TestLedger_RuntimeRoleAppendsAndReads(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	s := store.New(appRolePool(t, admin), zap.NewNop())
	stored := seedChain(t, ctx, s, 2)
	if stored[1].ChainSeq != 2 || stored[1].PrevHash != stored[0].CurrHash {
		t.Fatalf("runtime role chain: %+v", stored[1])
	}
	if v, err := s.VerifyChain(ctx, "tenant-1"); err != nil || !v.Valid {
		t.Fatalf("verify as runtime role: %+v %v", v, err)
	}
	if _, created, err := s.SealPackage(ctx, store.SealRequest{TenantID: "tenant-1", PrincipalID: "p", IdempotencyKey: "k", RequestHash: []byte{1}}); err != nil || !created {
		t.Fatalf("seal as runtime role: %v %v", created, err)
	}
}

func TestList_RegisterFacetsTotalsSearchAndCursor(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())

	mk := func(id, action, outcome string, mutate func(*domain.GovernanceDecision)) *domain.GovernanceDecision {
		d := sampleDecision(id)
		d.ActionType, d.Outcome = action, outcome
		if mutate != nil {
			mutate(&d)
		}
		stored, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return stored
	}
	parent := mk("p-1", "VENDOR_CONTRACT_RENEWAL", "PENDING", func(d *domain.GovernanceDecision) {
		d.Title, d.Priority, d.JurisdictionContext = strp("Approve vendor contract renewal"), strp("HIGH"), strp("GB")
		d.Tags = []string{"Vendor", "Renewal"}
	})
	mk("f-1", "VENDOR_CONTRACT_RENEWAL", "APPROVED", func(d *domain.GovernanceDecision) { d.CausationID = strp("p-1") })
	mk("d-1", "DATA_ACCESS_EXCEPTION", "DENIED", nil)
	mk("x-1", "SOMETHING_ODD", "MAYBE", nil)
	mk("e-1", "POLICY_UPDATE", "ESCALATED", func(d *domain.GovernanceDecision) { d.Domain = "AI_GOVERNANCE" })

	check := func(name string, p store.ListParams, want ...string) {
		t.Helper()
		p.TenantID = "tenant-1"
		res, err := s.List(ctx, p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, d := range res.Records {
			got = append(got, d.DecisionID)
		}
		if strings.Join(sorted(got), ",") != strings.Join(sorted(want), ",") || res.Total != int64(len(want)) {
			t.Fatalf("%s: got %v (total %d), want %v", name, got, res.Total, want)
		}
	}
	check("legal domain", store.ListParams{Domain: "LEGAL_CONTRACTS"}, "p-1", "f-1")
	check("explicit domain wins over classification", store.ListParams{Domain: "AI_GOVERNANCE"}, "e-1")
	check("data privacy", store.ListParams{Domain: "DATA_PRIVACY"}, "d-1")
	check("pending", store.ListParams{Category: "PENDING"}, "p-1")
	check("approved", store.ListParams{Category: "APPROVED"}, "f-1")
	check("unmapped outcome is OTHER", store.ListParams{Category: "OTHER"}, "x-1")
	check("priority", store.ListParams{Priority: "HIGH"}, "p-1")
	check("priority unset", store.ListParams{Priority: "UNSET"}, "f-1", "d-1", "x-1", "e-1")
	check("jurisdiction", store.ListParams{Jurisdiction: "GB"}, "p-1")
	check("follow-ups", store.ListParams{CausationID: "p-1"}, "f-1")
	check("free text", store.ListParams{Query: "renewal"}, "p-1", "f-1")
	check("free text treats % literally", store.ListParams{Query: "%"})
	check("display reference", store.ListParams{Query: parent.DisplayRef}, "p-1")

	got, _ := s.FindByID(ctx, "tenant-1", "p-1")
	if got.FollowUpCount != 1 || got.StatusCategory != "PENDING" || strings.Join(got.Tags, ",") != "Vendor,Renewal" {
		t.Fatalf("derived fields wrong: follow_ups=%d status=%s tags=%v", got.FollowUpCount, got.StatusCategory, got.Tags)
	}

	// Keyset paging returns every row exactly once, and the cursor starts on
	// page one (offset paging) as well as on later pages.
	seen := map[string]bool{}
	res, err := s.List(ctx, store.ListParams{TenantID: "tenant-1", Limit: 2})
	for page := 0; ; page++ {
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Records {
			if seen[d.DecisionID] {
				t.Fatalf("%s returned twice", d.DecisionID)
			}
			seen[d.DecisionID] = true
		}
		if res.NextCursor == nil {
			break
		}
		res, err = s.List(ctx, store.ListParams{TenantID: "tenant-1", Limit: 2, Cursor: res.NextCursor})
	}
	if len(seen) != 5 {
		t.Fatalf("cursor paging saw %d of 5 decisions", len(seen))
	}
}

func sorted(s []string) []string {
	out := append([]string{}, s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestSummary_CountsWindowsAndFacets(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	now := time.Now().UTC()

	rec := func(id, outcome string, ago time.Duration, mutate func(*domain.GovernanceDecision)) {
		d := sampleDecision(id)
		d.Outcome = outcome
		d.DecidedAt = now.Add(-ago).Truncate(time.Microsecond)
		if mutate != nil {
			mutate(&d)
		}
		if _, _, err := s.RecordDecision(ctx, store.DecisionRecord{Decision: d, Event: []byte(`{"event_type":"governance.decision.recorded"}`)}); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	rec("a", "GRANTED", time.Hour, func(d *domain.GovernanceDecision) { d.JurisdictionContext = strp("GB") })
	rec("b", "PENDING", 2*time.Hour, nil)
	rec("c", "ESCALATED", 3*time.Hour, nil)
	rec("d", "DENIED", 4*time.Hour, nil)
	rec("e", "APPROVED", 5*time.Hour, func(d *domain.GovernanceDecision) { d.CausationID = strp("b") })
	rec("old", "GRANTED", 40*24*time.Hour, nil)

	sum, err := s.Summary(ctx, store.SummaryParams{TenantID: "tenant-1", From: now.Add(-30 * 24 * time.Hour), To: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Totals.Total != 5 || sum.Totals.Approved != 2 || sum.Totals.Pending != 1 || sum.Totals.Escalated != 1 || sum.Totals.Denied != 1 {
		t.Fatalf("window totals: %+v", sum.Totals)
	}
	if sum.PreviousTotals.Total != 1 {
		t.Fatalf("previous window must hold the 40-day-old decision: %+v", sum.PreviousTotals)
	}
	if sum.OpenPending != 0 || sum.OpenEscalated != 1 {
		t.Fatalf("b was followed up by e, c was not: open pending %d escalated %d", sum.OpenPending, sum.OpenEscalated)
	}
	if len(sum.Trend) < 30 {
		t.Fatalf("trend must be dense, got %d points", len(sum.Trend))
	}
	var trendTotal int64
	for _, p := range sum.Trend {
		trendTotal += p.Total
	}
	if trendTotal != 5 {
		t.Fatalf("trend must add up to the window total, got %d", trendTotal)
	}
	if len(sum.ByDomain) != 9 || sum.ByDomain[0].Key != "PEOPLE_PAYROLL" || sum.ByDomain[0].Count != 5 {
		t.Fatalf("by_domain lists every domain, busiest first: %+v", sum.ByDomain)
	}
	// Six decisions queue twelve outbox rows: each wire event and its
	// canonical-name alias (group decision C3, rule R9).
	if sum.Chain.Length != 6 || sum.Outbox.Pending != 12 || len(sum.Facets.Domains) != 9 || len(sum.Facets.Jurisdictions) != 1 {
		t.Fatalf("ledger figures: chain %+v outbox %+v facets %+v", sum.Chain, sum.Outbox, sum.Facets)
	}
}

// The tenant-wide replay history joins each manifest to its decision. Its
// column list once split COALESCE(idempotency_key, ”) on the comma and every
// call answered 503 (found by the dependency e2e, 11 Nov).
func TestListReplayManifests_TenantWideWithDecisionReference(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	d := seedChain(t, ctx, s, 1)[0]
	for i, match := range []bool{true, false} {
		m := &domain.ReplayManifest{ReplayManifestID: fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i+1), DecisionID: d.DecisionID,
			PolicyVersionID: "pv-1", ReplayedOutcome: "GRANTED", OriginalOutcome: "DENIED", OutcomesMatch: match,
			ReplayedAt: time.Now().UTC().Add(time.Duration(i) * time.Second), ReplayedByPrincipalID: "auditor",
			TenantID: "tenant-1", IdempotencyKey: fmt.Sprintf("k-%d", i), RequestHash: []byte{1}}
		if _, _, err := s.CreateReplayManifest(ctx, m, nil); err != nil {
			t.Fatal(err)
		}
	}
	all, total, err := s.ListReplayManifests(ctx, store.ReplayListParams{TenantID: "tenant-1"})
	if err != nil || total != 2 || len(all) != 2 || all[0].DecisionDisplayRef != d.DisplayRef || all[0].DecisionActionType != "PAYROLL_RELEASE" {
		t.Fatalf("replay history: %d/%d %v %+v", len(all), total, err, all)
	}
	mism, total, err := s.ListReplayManifests(ctx, store.ReplayListParams{TenantID: "tenant-1", MismatchesOnly: true})
	if err != nil || total != 1 || len(mism) != 1 || mism[0].OutcomesMatch {
		t.Fatalf("mismatches only: %d/%d %v", len(mism), total, err)
	}
	if other, total, _ := s.ListReplayManifests(ctx, store.ReplayListParams{TenantID: "tenant-2"}); total != 0 || len(other) != 0 {
		t.Fatal("another tenant must see no replay")
	}
}
