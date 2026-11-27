package certreliance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func statusServer(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestCertificationStatus_FoldsBothServicesVocabulariesAndNeverGuesses(t *testing.T) {
	dq := Ref{ServiceDataQuality, "dqn_1"}
	rc := Ref{ServiceReconciliation, "rcr_2"}
	rev := `"revocation":{"revocation_id":"dqx_9","reason":"false pass"}`
	for name, tc := range map[string]struct {
		ref   Ref
		code  int
		body  string
		want  Status
		wantE error
	}{
		"dq active":                   {dq, 200, `{"status":"ACTIVE"}`, Status{State: StateActive}, nil},
		"dq stale":                    {dq, 200, `{"status":"STALE"}`, Status{State: StateStale}, nil},
		"dq revoked":                  {dq, 200, `{"status":"REVOKED",` + rev + `}`, Status{StateRevoked, "dqx_9", "false pass"}, nil},
		"recon certified":             {rc, 200, `{"certification_state":"CERTIFIED"}`, Status{State: StateActive}, nil},
		"recon superseded":            {rc, 200, `{"certification_state":"SUPERSEDED"}`, Status{State: StateSuperseded}, nil},
		"recon revoked":               {rc, 200, `{"certification_state":"REVOKED",` + rev + `}`, Status{StateRevoked, "dqx_9", "false pass"}, nil},
		"revoked, no revocation":      {dq, 200, `{"status":"REVOKED"}`, Status{}, ErrUnavailable},
		"revoked, no id":              {dq, 200, `{"status":"REVOKED","revocation":{"reason":"x"}}`, Status{}, ErrUnavailable},
		"unknown state":               {dq, 200, `{"status":"MOSTLY_FINE"}`, Status{}, ErrUnavailable},
		"missing state":               {dq, 200, `{}`, Status{}, ErrUnavailable},
		"the other field is not read": {dq, 200, `{"certification_state":"CERTIFIED"}`, Status{}, ErrUnavailable},
		"unreadable":                  {dq, 200, `not json`, Status{}, ErrUnavailable},
		"not found":                   {dq, 404, ``, Status{}, ErrUnknown},
		"forbidden":                   {dq, 403, ``, Status{}, ErrDenied},
		"server error":                {dq, 500, ``, Status{}, ErrUnavailable},
	} {
		s := statusServer(t, tc.code, tc.body)
		got, err := NewClient(s.URL, s.URL).CertificationStatus(context.Background(), "t", "p", "c", tc.ref)
		if tc.wantE != nil {
			if !errors.Is(err, tc.wantE) {
				t.Fatalf("%s: err = %v, want %v", name, err, tc.wantE)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("%s: %+v, %v; want %+v", name, got, err, tc.want)
		}
	}

	// A GET of the right route with the caller's identity; unconfigured or invalid never calls out.
	var seenPath, seenPrincipal, seenMethod string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenPrincipal, seenMethod = r.URL.Path, r.Header.Get("X-Principal-Id"), r.Method
		_, _ = w.Write([]byte(`{"certification_state":"CERTIFIED"}`))
	}))
	defer s.Close()
	c := NewClient(s.URL, s.URL)
	if _, err := c.CertificationStatus(context.Background(), "t", "alice", "c", rc); err != nil ||
		seenMethod != "GET" || seenPath != "/v1/reconciliation/runs/rcr_2/certification/blast-radius" || seenPrincipal != "alice" {
		t.Fatalf("%v %s %s %s", err, seenMethod, seenPath, seenPrincipal)
	}
	if _, err := NewClient("", "").CertificationStatus(context.Background(), "t", "p", "c", dq); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("not configured: %v", err)
	}
	if _, err := c.CertificationStatus(context.Background(), "t", "p", "c", Ref{ServiceDataQuality, "../x"}); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("invalid: %v", err)
	}
}
