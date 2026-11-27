package certwatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"zoiko.io/general-ledger-svc/internal/certreliance"
)

const (
	certA = "certification:data-quality-svc:dqn_a"
	certB = "certification:reconciliation-engine-svc:rcr_b"
	certC = "certification:data-quality-svc:dqn_c"
)

func ref(s string) certreliance.Ref { r, _ := certreliance.ParseRefs([]string{s}); return r[0] }

type memStore struct {
	Store     // anything the tests do not use panics
	watched   map[string]*Watched
	withdrawn []string // RecordWithdrawal calls, in order
	checks    map[string]string
	watchErr  error
}

func newMem() *memStore {
	return &memStore{watched: map[string]*Watched{}, checks: map[string]string{}}
}

func (m *memStore) Watch(_ context.Context, _, entity string, r certreliance.Ref) error {
	if m.watchErr != nil {
		return m.watchErr
	}
	if _, ok := m.watched[r.String()]; !ok {
		m.watched[r.String()] = &Watched{LegalEntityID: entity, Ref: r}
	}
	return nil
}

func (m *memStore) ListWatched(context.Context, string, string) ([]Watched, error) {
	var out []Watched
	for _, c := range []string{certA, certB, certC} {
		if w, ok := m.watched[c]; ok {
			out = append(out, *w)
		}
	}
	return out, nil
}

func (m *memStore) RecordCheck(_ context.Context, _, _ string, r certreliance.Ref, state string) error {
	m.checks[r.String()] = state
	return nil
}

func (m *memStore) RecordWithdrawal(_ context.Context, _, _ string, r certreliance.Ref, rev, reason string) (int, error) {
	m.withdrawn = append(m.withdrawn, r.String()+"|"+rev+"|"+reason)
	if w := m.watched[r.String()]; w.WithdrawnAt == nil {
		now := time.Now()
		w.WithdrawnAt, w.RevocationID, w.Reason = &now, rev, reason
	}
	return 2, nil // pretend two things cite it
}

type fakeStatus struct {
	by    map[string]certreliance.Status
	errs  map[string]error
	asked []string
}

func (f *fakeStatus) CertificationStatus(_ context.Context, _, _, _ string, r certreliance.Ref) (certreliance.Status, error) {
	f.asked = append(f.asked, r.String())
	if err := f.errs[r.String()]; err != nil {
		return certreliance.Status{}, err
	}
	return f.by[r.String()], nil
}

type fakeRecorder struct{ err error }

func (f fakeRecorder) RecordReliance(context.Context, string, string, string, certreliance.Ref, certreliance.Object) error {
	return f.err
}

func TestRecorder_WatchesAfterRecordingAndFailsClosedIfItCannot(t *testing.T) {
	m := newMem()
	r := &Recorder{Reliance: fakeRecorder{}, Store: m}
	if err := r.RecordReliance(context.Background(), "t", "p", "c", ref(certA), certreliance.Object{ID: "j", LegalEntityID: "e1"}); err != nil {
		t.Fatal(err)
	}
	if w := m.watched[certA]; w == nil || w.LegalEntityID != "e1" {
		t.Fatalf("not watched: %+v", m.watched)
	}
	// A refusal by the issuing service watches nothing: the journal is not relying on it.
	m2 := newMem()
	r2 := &Recorder{Reliance: fakeRecorder{err: certreliance.ErrNotInForce}, Store: m2}
	if err := r2.RecordReliance(context.Background(), "t", "p", "c", ref(certA), certreliance.Object{ID: "j", LegalEntityID: "e1"}); !errors.Is(err, certreliance.ErrNotInForce) || len(m2.watched) != 0 {
		t.Fatalf("%v %v", err, m2.watched)
	}
	// A watch that cannot be registered refuses the recording (so the posting): it would rest on a certification nobody checks again.
	m3 := newMem()
	m3.watchErr = errors.New("db down")
	r3 := &Recorder{Reliance: fakeRecorder{}, Store: m3}
	if err := r3.RecordReliance(context.Background(), "t", "p", "c", ref(certA), certreliance.Object{ID: "j", LegalEntityID: "e1"}); !errors.Is(err, certreliance.ErrUnavailable) {
		t.Fatalf("watch failure: %v", err)
	}
}

func TestSweeper_FlagsOnlyRevokedAndAFailedCheckNeverReadsAsInForce(t *testing.T) {
	m := newMem()
	for _, c := range []string{certA, certB, certC} {
		_ = m.Watch(context.Background(), "t", "e1", ref(c))
	}
	st := &fakeStatus{
		by: map[string]certreliance.Status{
			certA: {State: certreliance.StateRevoked, RevocationID: "dqx_1", Reason: "false pass"},
			certB: {State: certreliance.StateSuperseded},
		},
		errs: map[string]error{certC: certreliance.ErrUnavailable},
	}
	res, err := (&Sweeper{Store: m, Status: st}).Sweep(context.Background(), "t", "alice", "c", "e1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 3 || res.Withdrawn != 1 || res.NewFlags != 2 || res.Failed != 1 || len(res.Outcomes) != 3 {
		t.Fatalf("result: %+v", res)
	}
	if len(m.withdrawn) != 1 || m.withdrawn[0] != certA+"|dqx_1|false pass" {
		t.Fatalf("withdrawals: %v", m.withdrawn)
	}
	// SUPERSEDED is recorded and not flagged; the unreachable one is recorded as nothing, never as active.
	if m.checks[certB] != certreliance.StateSuperseded || m.checks[certA] != certreliance.StateRevoked {
		t.Fatalf("checks: %v", m.checks)
	}
	if _, ok := m.checks[certC]; ok {
		t.Fatalf("a certification that could not be checked was recorded as %q", m.checks[certC])
	}
	for _, o := range res.Outcomes {
		if o.Certification == certC && (o.Error == "" || o.State != "") {
			t.Fatalf("unreachable outcome: %+v", o)
		}
	}
}

func TestSweeper_AWithdrawnCertificationIsRescannedNotAskedAgain(t *testing.T) {
	m := newMem()
	_ = m.Watch(context.Background(), "t", "e1", ref(certA))
	now := time.Now()
	m.watched[certA].WithdrawnAt, m.watched[certA].RevocationID, m.watched[certA].Reason = &now, "dqx_1", "false pass"
	st := &fakeStatus{}
	res, err := (&Sweeper{Store: m, Status: st}).Sweep(context.Background(), "t", "alice", "c", "e1")
	if err != nil || len(st.asked) != 0 {
		t.Fatalf("%v, asked %v", err, st.asked)
	}
	// Re-scanned with the FIRST revocation, so a journal posted after the withdrawal is still caught.
	if len(m.withdrawn) != 1 || m.withdrawn[0] != certA+"|dqx_1|false pass" || res.Withdrawn != 1 || res.NewFlags != 2 {
		t.Fatalf("%v %+v", m.withdrawn, res)
	}
}
