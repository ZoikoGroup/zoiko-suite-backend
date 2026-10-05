package privacy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/domain"
)

func req() Request {
	return Request{TenantID: "t1", PrincipalID: "sender", CorrelationID: "c1", SubjectRef: "recipient", ActivityID: "act-1", PurposeID: "pur-1"}
}

// capture hands the request a test server saw back to the test goroutine under a lock, so
// the race detector sees the happens-before the network hides.
type capture struct {
	mu  sync.Mutex
	req *http.Request
	raw []byte
}

func (c *capture) get() (*http.Request, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.req, c.raw
}

func serve(t *testing.T, status int, body string) (*Client, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		cap.mu.Lock()
		cap.req, cap.raw = r.Clone(r.Context()), buf[:n]
		cap.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, time.Second, nil), cap
}

func TestClient_AsksTheDecisionServiceAndReadsAPermit(t *testing.T) {
	c, cap := serve(t, 200, `{"decision_id":"d-1","result":"PERMIT","reason_codes":[]}`)
	d, err := c.Decide(context.Background(), req())
	require.NoError(t, err)
	assert.Equal(t, "PERMIT", d.Result)
	assert.Equal(t, "d-1", d.DecisionID)

	got, raw := cap.get()
	assert.Equal(t, "/v1/privacy/decisions", got.URL.Path)
	assert.Equal(t, "t1", got.Header.Get("X-Tenant-Id"))
	assert.Equal(t, "sender", got.Header.Get("X-Principal-Id"))
	assert.Equal(t, "c1", got.Header.Get("X-Correlation-ID"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Equal(t, "recipient", body["subject_ref"])
	assert.Equal(t, "act-1", body["processing_activity_id"])
	assert.Equal(t, "pur-1", body["purpose_id"])
	assert.Equal(t, "USE", body["proposed_operation"])
}

func TestClient_AnythingButARecognisableDecisionIsUnavailable(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"server error":      {500, `{"decision_id":"d","result":"PERMIT"}`},
		"unauthorised":      {401, `{"decision_id":"d","result":"PERMIT"}`},
		"not json":          {200, `<html>`},
		"unknown result":    {200, `{"decision_id":"d","result":"MAYBE"}`},
		"empty result":      {200, `{"decision_id":"d"}`},
		"no decision id":    {200, `{"result":"PERMIT"}`},
		"permit lowercased": {200, `{"decision_id":"d","result":"permit"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := serve(t, tc.status, tc.body)
			d, err := c.Decide(context.Background(), req())
			assert.Nil(t, d)
			assert.ErrorIs(t, err, ErrUnavailable)
			v := Judge(d, err)
			assert.False(t, v.Allow, "never permission")
			assert.True(t, v.Retryable)
			assert.Equal(t, ResultUnavailable, v.Result)
		})
	}
}

func TestClient_UnreachableAndSlowAreUnavailable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", time.Second, nil)
	_, err := c.Decide(context.Background(), req())
	assert.ErrorIs(t, err, ErrUnavailable)

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	_, err = NewClient(slow.URL, 100*time.Millisecond, nil).Decide(context.Background(), req())
	assert.ErrorIs(t, err, ErrUnavailable, "a timeout is a refusal, never a permission")
}

func TestClient_IncompleteRequestIsNotSent(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", time.Second, nil)
	for _, mut := range []func(*Request){
		func(r *Request) { r.TenantID = "" }, func(r *Request) { r.PrincipalID = "" },
		func(r *Request) { r.SubjectRef = "" }, func(r *Request) { r.ActivityID = "" }, func(r *Request) { r.PurposeID = "" },
	} {
		r := req()
		mut(&r)
		_, err := c.Decide(context.Background(), r)
		require.ErrorIs(t, err, ErrUnavailable)
		assert.Contains(t, err.Error(), "incomplete")
	}
}

func TestJudge_OnlyPermitAllows(t *testing.T) {
	cases := []struct {
		result    string
		allow     bool
		retryable bool
		contains  string
	}{
		{ResultPermit, true, false, ""},
		{ResultRestrict, false, false, "cannot enforce"},
		{ResultBlock, false, false, "blocks"},
		{ResultReviewRequired, false, false, "review"},
		{ResultIndeterminate, false, true, "indeterminate"},
		{"SOMETHING_NEW", false, true, "unrecognised"},
	}
	for _, tc := range cases {
		v := Judge(&Decision{DecisionID: "d", Result: tc.result, ReasonCodes: []string{"PRV-001"},
			Constraints: []Constraint{{Type: "MINIMISE"}}}, nil)
		assert.Equal(t, tc.allow, v.Allow, tc.result)
		assert.Equal(t, tc.retryable, v.Retryable, tc.result)
		assert.Equal(t, "d", v.DecisionID)
		if !tc.allow {
			assert.Contains(t, v.Reason, "NCD-008", tc.result)
			assert.Contains(t, strings.ToLower(v.Reason), tc.contains, tc.result)
		}
	}
	assert.Contains(t, Judge(&Decision{DecisionID: "d", Result: ResultRestrict, Constraints: []Constraint{{Type: "MINIMISE"}}}, nil).Reason, "MINIMISE")
	assert.False(t, Judge(nil, nil).Allow)
	assert.False(t, Judge(&Decision{DecisionID: "d", Result: ResultPermit}, errors.New("x")).Allow, "an error wins over a stale decision")
}

type fakeVersions struct {
	v   *domain.IntentVersion
	err error
	got string
}

func (f *fakeVersions) GetIntentVersion(_ context.Context, id string) (*domain.IntentVersion, error) {
	f.got = id
	return f.v, f.err
}

type fakeDecider struct {
	d     *Decision
	err   error
	calls int
	got   Request
}

func (f *fakeDecider) Decide(_ context.Context, r Request) (*Decision, error) {
	f.calls++
	f.got = r
	return f.d, f.err
}

func sp(s string) *string { return &s }

func notif() domain.Notification {
	return domain.Notification{NotificationID: "n1", TenantID: "t1", RecipientPrincipalID: "recipient",
		CreatedByPrincipalID: "sender", CorrelationID: "c1", IntentVersionID: "iv-1"}
}

func TestGate_NoIntentMeansNoBindingToEnforce(t *testing.T) {
	d := &fakeDecider{}
	g, err := NewGate(d, &fakeVersions{}, nil)
	require.NoError(t, err)
	n := notif()
	n.IntentVersionID = ""
	o := g.Check(context.Background(), n)
	assert.False(t, o.Applies)
	assert.Zero(t, d.calls)
}

func TestGate_AsksAboutTheIntentsBindingAndTheRecipient(t *testing.T) {
	d := &fakeDecider{d: &Decision{DecisionID: "d-9", Result: ResultPermit}}
	v := &fakeVersions{v: &domain.IntentVersion{PrivacyActivityID: sp("act-1"), PrivacyPurposeID: sp("pur-1")}}
	g, _ := NewGate(d, v, nil)
	o := g.Check(context.Background(), notif())
	assert.True(t, o.Applies)
	assert.True(t, o.Allow)
	assert.Equal(t, "d-9", o.DecisionID)
	assert.Equal(t, "iv-1", v.got)
	assert.Equal(t, Request{TenantID: "t1", PrincipalID: "sender", CorrelationID: "c1", SubjectRef: "recipient", ActivityID: "act-1", PurposeID: "pur-1"}, d.got)
}

func TestGate_FailsClosedWhenItCannotAsk(t *testing.T) {
	// Intent version unreadable.
	d := &fakeDecider{d: &Decision{DecisionID: "d", Result: ResultPermit}}
	g, _ := NewGate(d, &fakeVersions{err: errors.New("db down")}, nil)
	o := g.Check(context.Background(), notif())
	assert.True(t, o.Applies)
	assert.False(t, o.Allow)
	assert.True(t, o.Retryable)
	assert.Equal(t, ResultUnavailable, o.Result)
	assert.Zero(t, d.calls)

	// No privacy binding: refused, terminal, never assumed.
	g, _ = NewGate(d, &fakeVersions{v: &domain.IntentVersion{}}, nil)
	o = g.Check(context.Background(), notif())
	assert.False(t, o.Allow)
	assert.False(t, o.Retryable)
	assert.Equal(t, ResultNotBound, o.Result)
	assert.Zero(t, d.calls, "nothing to ask")

	// Authority down.
	bound := &fakeVersions{v: &domain.IntentVersion{PrivacyActivityID: sp("a"), PrivacyPurposeID: sp("p")}}
	g, _ = NewGate(&fakeDecider{err: ErrUnavailable}, bound, nil)
	o = g.Check(context.Background(), notif())
	assert.False(t, o.Allow)
	assert.True(t, o.Retryable)
}

func TestNewGate_RefusesMissingCollaborators(t *testing.T) {
	_, err := NewGate(nil, &fakeVersions{}, nil)
	assert.Error(t, err)
	_, err = NewGate(&fakeDecider{}, nil, nil)
	assert.Error(t, err)
}
