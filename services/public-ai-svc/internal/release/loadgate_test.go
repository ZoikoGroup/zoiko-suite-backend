package release

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"zoiko.io/public-ai-svc/internal/claims"
	"zoiko.io/public-ai-svc/internal/signing"
	"zoiko.io/public-ai-svc/internal/store"
)

// publishClaims signs m as release id (bypassing BuildClaims, as a tampered
// or hand-built artifact would) and promotes it.
func publishClaims(t *testing.T, e env, id string, mut func(*claims.Manifest)) {
	t.Helper()
	body, _, err := BuildClaims(filepath.Join(fixtures, "claims.json"), id, now, e.key)
	if err != nil {
		t.Fatal(err)
	}
	var m claims.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	mut(&m)
	body, _ = json.Marshal(m)
	sig, err := signing.Sign(e.key, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, e.st, e.pub, id, claims.KindClaims, body, sig); err != nil {
		t.Fatal(err)
	}
	if err := Promote(ctx, e.st, claims.KindClaims, id, nil); err != nil {
		t.Fatal(err)
	}
}

// Doc 03 §6/§12, CT-043: a signed claim release that fails registry
// validation or carries synthetic data is refused at load, like a KB release.
func TestClaimsReleaseGatedAtLoad(t *testing.T) {
	cases := []struct {
		name           string
		mut            func(*claims.Manifest)
		allowSynthetic bool
		want           error
	}{
		{"duplicate claim id", func(m *claims.Manifest) { m.Claims = append(m.Claims, m.Claims[0]) }, false, ErrInvalidClaims},
		{"one approver", func(m *claims.Manifest) {
			for i := range m.Claims {
				if m.Claims[i].Status == claims.StatusActive {
					m.Claims[i].ApprovalRefs = []string{"a", "a"}
					return
				}
			}
		}, false, ErrInvalidClaims},
		{"synthetic claim", func(m *claims.Manifest) { m.Claims[0].Synthetic = true }, false, ErrSyntheticInRelease},
		{"synthetic allowed in dev", func(m *claims.Manifest) { m.Claims[0].Synthetic = true }, true, nil},
	}
	for i, c := range cases {
		e := setup(t)
		e.rt.AllowSynthetic = c.allowSynthetic
		publishClaims(t, e, "claims-bad-"+string(rune('a'+i)), c.mut)
		err := e.rt.Refresh(ctx)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: refresh = %v, want %v", c.name, err, c.want)
		}
		if _, serr := e.rt.Snapshot(); (serr == nil) != (c.want == nil) {
			t.Errorf("%s: snapshot err = %v", c.name, serr)
		}
	}
}

// Doc 03 §19: a stored answer citing a source version or claim revoked
// since is detected, whether or not the runtime has refreshed yet.
func TestCitationsRevoked(t *testing.T) {
	e := setup(t)
	snap, _ := e.rt.Snapshot()
	var srcID, verID string
	for _, s := range snap.kbManifest.Sources {
		srcID, verID = s.SourceID, s.SourceVersionID
		break
	}
	if got, err := e.rt.CitationsRevoked(ctx, []string{verID}, []string{"c-x"}); got || err != nil {
		t.Fatalf("nothing revoked yet: %v %v", got, err)
	}
	// Whole source revoked in the ledger, runtime not refreshed.
	if _, err := Revoke(ctx, e.st, "source", srcID, "WITHDRAWN", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.rt.CitationsRevoked(ctx, []string{verID}, nil); !got {
		t.Fatal("source revocation in the ledger must be seen before refresh")
	}
	if _, err := Revoke(ctx, e.st, "claim", "c-x", "WITHDRAWN", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.rt.CitationsRevoked(ctx, nil, []string{"c-x"}); !got {
		t.Fatal("claim revocation must be seen after refresh")
	}
}

// Doc 03 §11: a rollback needs a completed promotion of that release in the
// control audit; a promotion that failed (compensated) does not count.
func TestRollbackNeedsPriorPromotion(t *testing.T) {
	st := store.NewMemory()
	op := Operator{Actor: "sre-1", Approval: "CHG-1"}
	if err := VerifyRollback(ctx, st, "kb", "kb-1"); !errors.Is(err, ErrNoPriorPromotion) {
		t.Fatalf("never promoted: %v", err)
	}
	_ = st.AppendAudit(ctx, store.AuditEntry{Action: AuditPromote, Target: "kb", Actor: "a", Approval: "b", AfterHash: store.Hash([]byte("kb-1"))})
	_ = st.AppendAudit(ctx, store.AuditEntry{Action: AuditPromoteFailed, Target: "kb", Actor: "a", Approval: "b", AfterHash: store.Hash([]byte("kb-1"))})
	if err := VerifyRollback(ctx, st, "kb", "kb-1"); !errors.Is(err, ErrNoPriorPromotion) {
		t.Fatalf("compensated promotion must not count: %v", err)
	}
	if err := VerifyRollback(ctx, st, "claims", "kb-1"); !errors.Is(err, ErrNoPriorPromotion) {
		t.Fatalf("other kind must not count: %v", err)
	}
	if err := st.PutRelease(ctx, store.Release{ReleaseID: "kb-1", Kind: "kb", Manifest: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if err := PromoteAudited(ctx, st, op, "kb", "kb-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRollback(ctx, st, "kb", "kb-1"); err != nil {
		t.Fatalf("promoted release: %v", err)
	}
}

// failingPointer refuses to move the pointer.
type failingPointer struct{ *store.Memory }

func (failingPointer) SetPointer(context.Context, string, string, []store.OutboxEvent) error {
	return errors.New("db down")
}

// Doc 08 OPR-033: the audit is written before the pointer moves; a move
// that then fails is compensated, so the ledger never claims a promotion.
func TestPromoteAuditedCompensatesFailedMove(t *testing.T) {
	st := failingPointer{store.NewMemory()}
	op := Operator{Actor: "sre-1", Approval: "CHG-1"}
	if err := PromoteAudited(ctx, st, op, "kb", "kb-1", nil); err == nil {
		t.Fatal("failed move must be reported")
	}
	got, _ := st.ListAudit(ctx, 10)
	if len(got) != 2 || got[0].Action != AuditPromoteFailed || got[1].Action != AuditPromote {
		t.Fatalf("audit: %+v", got)
	}
	if err := VerifyRollback(ctx, st, "kb", "kb-1"); err == nil {
		t.Fatal("a failed promotion is no basis for rollback")
	}
}
