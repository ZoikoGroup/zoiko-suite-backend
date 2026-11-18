package sealer

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/store"
)

type fakeStore struct {
	tenants  []store.UnsealedTenant
	listErr  error
	sealErr  map[string]error
	requests []store.SealRequest
	packages map[string]*domain.EvidencePackage // by tenant + key
}

func (f *fakeStore) UnsealedTenants(context.Context) ([]store.UnsealedTenant, error) {
	return f.tenants, f.listErr
}

func (f *fakeStore) SealPackage(_ context.Context, req store.SealRequest) (*domain.EvidencePackage, bool, error) {
	f.requests = append(f.requests, req)
	if err := f.sealErr[req.TenantID]; err != nil {
		return nil, false, err
	}
	if f.packages == nil {
		f.packages = map[string]*domain.EvidencePackage{}
	}
	k := req.TenantID + "|" + req.IdempotencyKey
	if p, ok := f.packages[k]; ok {
		return p, false, nil
	}
	p := &domain.EvidencePackage{PackageID: "pkg-" + k, ToSeq: 1}
	f.packages[k] = p
	return p, true, nil
}

type countObs map[string]int

func (c countObs) ObserveSeal(o string) { c[o]++ }

// Every tenant with unsealed decisions is sealed, as the sealer's principal,
// under a key derived from the head it saw.
func TestRunOnce_SealsEveryUnsealedTenant(t *testing.T) {
	fs := &fakeStore{tenants: []store.UnsealedTenant{{TenantID: "t1", HeadSeq: 5}, {TenantID: "t2", HeadSeq: 9, SealedTo: 3}}}
	obs := countObs{}
	res, err := New(fs, "svc-governance-decision-log", zap.NewNop(), obs).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Sealed != 2 || obs[OutcomeSealed] != 2 {
		t.Fatalf("sealed %d (observed %d), want 2", res.Sealed, obs[OutcomeSealed])
	}
	for _, r := range fs.requests {
		if r.PrincipalID != "svc-governance-decision-log" || r.IdempotencyKey == "" || len(r.RequestHash) == 0 || r.ThroughDecisionID != "" {
			t.Fatalf("seal request %+v", r)
		}
	}
	if fs.requests[0].IdempotencyKey != "scheduled-seal:5" || fs.requests[1].IdempotencyKey != "scheduled-seal:9" {
		t.Fatalf("keys %q %q", fs.requests[0].IdempotencyKey, fs.requests[1].IdempotencyKey)
	}
}

// Two replicas on the same tick see the same head, use the same key, and the
// second gets the first's package back: one package, one "sealed".
func TestRunOnce_SecondReplicaSameTickSealsNothingNew(t *testing.T) {
	fs := &fakeStore{tenants: []store.UnsealedTenant{{TenantID: "t1", HeadSeq: 5}}}
	obs := countObs{}
	s := New(fs, "svc", zap.NewNop(), obs)
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Sealed != 0 || obs[OutcomeSealed] != 1 || len(fs.packages) != 1 {
		t.Fatalf("second run sealed %d, observed %d, packages %d", res.Sealed, obs[OutcomeSealed], len(fs.packages))
	}
}

// A broken chain is never certified: the seal is refused, counted and the
// other tenants are still sealed. NOTHING_TO_SEAL (raced) is not an error.
func TestRunOnce_ChainBrokenReportedOthersStillSealed(t *testing.T) {
	fs := &fakeStore{
		tenants: []store.UnsealedTenant{{TenantID: "bad", HeadSeq: 3}, {TenantID: "raced", HeadSeq: 2}, {TenantID: "ok", HeadSeq: 4}, {TenantID: "down", HeadSeq: 1}},
		sealErr: map[string]error{"bad": domain.ErrChainBroken, "raced": domain.ErrNothingToSeal, "down": errors.New("db gone")},
	}
	obs := countObs{}
	res, err := New(fs, "svc", zap.NewNop(), obs).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Sealed != 1 || res.ChainBroken != 1 || res.Failed != 1 {
		t.Fatalf("result %+v", res)
	}
	if obs[OutcomeChainBroken] != 1 || obs[OutcomeError] != 1 || obs[OutcomeSealed] != 1 {
		t.Fatalf("observed %v", obs)
	}
}

func TestRunOnce_ListFailureIsAnError(t *testing.T) {
	obs := countObs{}
	if _, err := New(&fakeStore{listErr: errors.New("db gone")}, "svc", zap.NewNop(), obs).RunOnce(context.Background()); err == nil {
		t.Fatal("a failed listing must be reported")
	}
	if obs[OutcomeError] != 1 {
		t.Fatalf("observed %v", obs)
	}
}
