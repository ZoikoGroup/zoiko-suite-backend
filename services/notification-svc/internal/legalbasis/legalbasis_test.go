package legalbasis

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakePolicy struct {
	versionStatus int
	version       string
	policyStatus  int
	policy        string
	tenant        string
	principal     string
	paths         []string
}

func (f *fakePolicy) handler(w http.ResponseWriter, r *http.Request) {
	f.paths = append(f.paths, r.URL.Path)
	f.tenant, f.principal = r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id")
	if strings.HasPrefix(r.URL.Path, "/v1/policy-versions/") {
		w.WriteHeader(f.versionStatus)
		_, _ = w.Write([]byte(f.version))
		return
	}
	w.WriteHeader(f.policyStatus)
	_, _ = w.Write([]byte(f.policy))
}

const activeVersion = `{"policy_version_id":"v-1","policy_id":"p-1","tenant_id":null,"effective_from":"2026-01-01T00:00:00Z","effective_to":null,"version_status":"ACTIVE","version_number":3}`

func okPolicy() *fakePolicy {
	return &fakePolicy{versionStatus: 200, version: activeVersion, policyStatus: 200, policy: `{"policy_id":"p-1","policy_code":"UK-CONSULT-7","policy_type":"LEGAL_BASIS"}`}
}

func verify(t *testing.T, f *fakePolicy) (*Basis, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "notification-svc", 0, nil).Verify(context.Background(), "t1", "corr", "v-1", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
}

func TestVerify_AcceptsAnActiveInForceLegalBasisAndAsksAsTheWorkloadUnderTheTenant(t *testing.T) {
	f := okPolicy()
	b, err := verify(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if b.PolicyCode != "UK-CONSULT-7" || b.PolicyVersionID != "v-1" || b.VersionNumber != 3 {
		t.Fatalf("%+v", b)
	}
	if f.tenant != "t1" || f.principal != "notification-svc" || len(f.paths) != 2 || f.paths[0] != "/v1/policy-versions/v-1" || f.paths[1] != "/v1/policies/p-1" {
		t.Fatalf("tenant=%s principal=%s paths=%v", f.tenant, f.principal, f.paths)
	}
}

func TestVerify_RefusesAnythingThatIsNotAnActiveInForceLegalBasisForTheTenant(t *testing.T) {
	cases := map[string]func(f *fakePolicy){
		"unknown version":  func(f *fakePolicy) { f.versionStatus = 404 },
		"superseded":       func(f *fakePolicy) { f.version = strings.Replace(activeVersion, `"ACTIVE"`, `"SUPERSEDED"`, 1) },
		"draft":            func(f *fakePolicy) { f.version = strings.Replace(activeVersion, `"ACTIVE"`, `"DRAFT"`, 1) },
		"not yet in force": func(f *fakePolicy) { f.version = strings.Replace(activeVersion, "2026-01-01", "2026-12-01", 1) },
		"ended": func(f *fakePolicy) {
			f.version = strings.Replace(activeVersion, `"effective_to":null`, `"effective_to":"2026-06-01T00:00:00Z"`, 1)
		},
		"another tenant": func(f *fakePolicy) {
			f.version = strings.Replace(activeVersion, `"tenant_id":null`, `"tenant_id":"t2"`, 1)
		},
		"not a legal basis": func(f *fakePolicy) { f.policy = strings.Replace(f.policy, "LEGAL_BASIS", "APPROVAL_THRESHOLD", 1) },
	}
	for name, mut := range cases {
		f := okPolicy()
		mut(f)
		if _, err := verify(t, f); !errors.Is(err, ErrNotUsable) {
			t.Errorf("%s: want ErrNotUsable, got %v", name, err)
		}
	}
	// This tenant own version is fine.
	f := okPolicy()
	f.version = strings.Replace(activeVersion, `"tenant_id":null`, `"tenant_id":"T1"`, 1)
	if _, err := verify(t, f); err != nil {
		t.Fatalf("a version of this tenant is usable: %v", err)
	}
}

func TestVerify_AnOutageOrALackOfGrantIsNeverAcceptance(t *testing.T) {
	for name, mut := range map[string]func(f *fakePolicy){
		"version 500": func(f *fakePolicy) { f.versionStatus = 500 },
		"version 403": func(f *fakePolicy) { f.versionStatus = 403 },
		"version 401": func(f *fakePolicy) { f.versionStatus = 401 },
		"policy 503":  func(f *fakePolicy) { f.policyStatus = 503 },
		"policy 404":  func(f *fakePolicy) { f.policyStatus = 404 },
		"unreadable":  func(f *fakePolicy) { f.version = "not json" },
	} {
		f := okPolicy()
		mut(f)
		if _, err := verify(t, f); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: want ErrUnavailable, got %v", name, err)
		}
	}
	if _, err := NewClient("http://127.0.0.1:1", "n", 0, nil).Verify(context.Background(), "t", "", "v", time.Now()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}
