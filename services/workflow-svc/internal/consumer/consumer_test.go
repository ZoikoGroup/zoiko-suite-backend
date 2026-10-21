package consumer_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/consumer"
)

type fakeStore struct {
	seen      map[string]bool
	marked    []string
	forgotten []string
	markErr   error
}

func (f *fakeStore) RecordConsumedEvent(_ context.Context, id, _, _ string) (bool, error) {
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[id] {
		return false, nil
	}
	f.seen[id] = true
	return true, nil
}
func (f *fakeStore) ForgetConsumedEvent(_ context.Context, id string) error {
	delete(f.seen, id)
	f.forgotten = append(f.forgotten, id)
	return nil
}
func (f *fakeStore) MarkApproverRecheck(_ context.Context, tenant, principal string) (int, error) {
	if f.markErr != nil {
		return 0, f.markErr
	}
	f.marked = append(f.marked, tenant+"/"+principal)
	return 1, nil
}

func msg(t *testing.T, id, typ, tenant string, payload map[string]any) []byte {
	t.Helper()
	p, _ := json.Marshal(payload)
	b, _ := json.Marshal(map[string]any{"event_id": id, "event_type": typ, "tenant_id": tenant, "payload": json.RawMessage(p)})
	return b
}

func TestApply_FlagsTheAffectedApprovers(t *testing.T) {
	for _, tc := range []struct {
		typ     string
		payload map[string]any
		want    []string
	}{
		{"policy.updated", nil, []string{"t-1/"}},
		{"policy.version.activated", nil, []string{"t-1/"}},
		{"authority.revoked", map[string]any{"delegator_principal_id": "boss", "delegate_principal_id": "cover"}, []string{"t-1/boss", "t-1/cover"}},
		{"authority.delegated", map[string]any{"delegator_principal_id": "boss", "delegate_principal_id": "cover"}, []string{"t-1/boss", "t-1/cover"}},
		{"authorization.denied", map[string]any{"principal_id": "approver-1"}, []string{"t-1/approver-1"}},
	} {
		st := &fakeStore{}
		if err := consumer.Apply(context.Background(), st, msg(t, "e-"+tc.typ, tc.typ, "t-1", tc.payload), zap.NewNop()); err != nil {
			t.Fatalf("%s: %v", tc.typ, err)
		}
		if len(st.marked) != len(tc.want) {
			t.Fatalf("%s: marked %v, want %v", tc.typ, st.marked, tc.want)
		}
		for i := range tc.want {
			if st.marked[i] != tc.want[i] {
				t.Errorf("%s: marked %v, want %v", tc.typ, st.marked, tc.want)
			}
		}
	}
}

func TestApply_SkipsOtherEventsAndRedeliveries(t *testing.T) {
	st := &fakeStore{}
	_ = consumer.Apply(context.Background(), st, msg(t, "e-1", "policy.created", "t-1", nil), zap.NewNop())
	_ = consumer.Apply(context.Background(), st, []byte("not json"), zap.NewNop())
	_ = consumer.Apply(context.Background(), st, msg(t, "e-2", "policy.updated", "", nil), zap.NewNop())
	if len(st.marked) != 0 {
		t.Fatalf("marked %v for events this service does not act on", st.marked)
	}
	m := msg(t, "e-3", "authorization.denied", "t-1", map[string]any{"principal_id": "a"})
	_ = consumer.Apply(context.Background(), st, m, zap.NewNop())
	_ = consumer.Apply(context.Background(), st, m, zap.NewNop())
	if len(st.marked) != 1 {
		t.Fatalf("a redelivery was applied twice: %v", st.marked)
	}
}

// A failed apply releases its claim and reports the error, so the message is
// not committed and its redelivery is applied.
func TestApply_FailureReleasesTheClaim(t *testing.T) {
	st := &fakeStore{markErr: errors.New("db down")}
	m := msg(t, "e-1", "policy.updated", "t-1", nil)
	if err := consumer.Apply(context.Background(), st, m, zap.NewNop()); err == nil {
		t.Fatal("a failed apply reported success; the message would be committed and the re-check lost")
	}
	if len(st.forgotten) != 1 || st.seen["e-1"] {
		t.Fatalf("claim not released: forgotten=%v seen=%v", st.forgotten, st.seen)
	}
	st.markErr = nil
	if err := consumer.Apply(context.Background(), st, m, zap.NewNop()); err != nil || len(st.marked) != 1 {
		t.Fatalf("redelivery not applied: err=%v marked=%v", err, st.marked)
	}
}
