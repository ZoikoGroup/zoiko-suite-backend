package catalogref

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

const (
	oid = "11111111-1111-1111-1111-111111111111"
	vid = "22222222-2222-2222-2222-222222222222"
)

func TestParse(t *testing.T) {
	if _, _, ok, err := Parse("CAT-14"); ok || err != nil {
		t.Fatalf("opaque ref must pass through untouched, got ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := Parse(""); ok || err != nil {
		t.Fatalf("empty ref must pass through, got ok=%v err=%v", ok, err)
	}
	o, v, ok, err := Parse("catalog:" + oid + ":" + vid)
	if !ok || err != nil || o != oid || v != vid {
		t.Fatalf("expected parse, got %s %s %v %v", o, v, ok, err)
	}
	for _, bad := range []string{"catalog:", "catalog:x:y", "catalog:" + oid, "catalog:" + oid + ":" + vid + ":extra"} {
		if _, _, ok, err := Parse(bad); ok || err != ErrMalformed {
			t.Fatalf("%q: expected ErrMalformed, got ok=%v err=%v", bad, ok, err)
		}
	}
}

func serve(t *testing.T, status int, body string) *HTTPChecker {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/catalog/offerings/"+oid+"/versions/"+vid+"/usability" || r.Header.Get("X-Tenant-Id") != "t1" || r.Header.Get("X-Principal-Id") == "" {
			t.Errorf("unexpected request %s tenant=%q principal=%q", r.URL.Path, r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id"))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return NewHTTPChecker(s.URL)
}

var caller = svcclient.Caller{TenantID: "t1", PrincipalID: "system:ar-catalog"}

func TestCheck_Usable(t *testing.T) {
	r, err := serve(t, 200, `{"usable":true,"status":"ACTIVE","missing_mappings":[],"reason":""}`).Check(context.Background(), caller, oid, vid)
	if err != nil || !r.Found || !r.Usable {
		t.Fatalf("expected usable, got %+v %v", r, err)
	}
}

func TestCheck_NotUsable(t *testing.T) {
	r, err := serve(t, 200, `{"usable":false,"status":"SUSPENDED","missing_mappings":[],"reason":"version status is SUSPENDED, not ACTIVE"}`).Check(context.Background(), caller, oid, vid)
	if err != nil || !r.Found || r.Usable || r.Status != "SUSPENDED" {
		t.Fatalf("expected not usable, got %+v %v", r, err)
	}
}

func TestCheck_NotFound(t *testing.T) {
	r, err := serve(t, 404, `{}`).Check(context.Background(), caller, oid, vid)
	if err != nil || r.Found {
		t.Fatalf("expected found=false, got %+v %v", r, err)
	}
}

func TestCheck_FailsClosed(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{500, ``}, {503, ``}, {401, ``}, {403, ``}, {200, `not json`}} {
		if _, err := serve(t, tc.status, tc.body).Check(context.Background(), caller, oid, vid); err != ErrUnavailable {
			t.Fatalf("status %d body %q: expected ErrUnavailable, got %v", tc.status, tc.body, err)
		}
	}
	if _, err := NewHTTPChecker("http://127.0.0.1:1").Check(context.Background(), caller, oid, vid); err != ErrUnavailable {
		t.Fatalf("transport error must be ErrUnavailable, got %v", err)
	}
}

type fakeChecker struct {
	res  *Result
	err  error
	n    int
	last [2]string
}

func (f *fakeChecker) Check(_ context.Context, _ svcclient.Caller, o, v string) (*Result, error) {
	f.n++
	f.last = [2]string{o, v}
	return f.res, f.err
}

func TestRequire(t *testing.T) {
	ref := "catalog:" + oid + ":" + vid
	ctx := context.Background()
	code := func(err error) string {
		if p, ok := err.(*kit.Problem); ok {
			return p.Code
		}
		return "<nil>"
	}
	f := &fakeChecker{}
	if err := Require(ctx, f, caller, "CAT-14"); err != nil || f.n != 0 {
		t.Fatalf("opaque ref must not call the catalog, err=%v calls=%d", err, f.n)
	}
	if err := Require(ctx, nil, caller, ""); err != nil {
		t.Fatalf("empty ref must pass, got %v", err)
	}
	if err := Require(ctx, nil, caller, ref); code(err) != kit.CodeDependencyUnavailable {
		t.Fatalf("catalog ref with no checker must fail closed, got %v", err)
	}
	if err := Require(ctx, f, caller, "catalog:bad"); code(err) != kit.CodeValidationFailed {
		t.Fatalf("malformed catalog ref must be a validation error, got %v", err)
	}
	f = &fakeChecker{err: ErrUnavailable}
	if err := Require(ctx, f, caller, ref); code(err) != kit.CodeDependencyUnavailable {
		t.Fatalf("unavailable catalog must fail closed, got %v", err)
	}
	f = &fakeChecker{res: &Result{Found: false}}
	if err := Require(ctx, f, caller, ref); code(err) != kit.CodeCatalogVersionInvalid {
		t.Fatalf("unknown version must be CATALOG_VERSION_INVALID, got %v", err)
	}
	f = &fakeChecker{res: &Result{Found: true, Usable: false, Reason: "version status is SUSPENDED, not ACTIVE"}}
	if err := Require(ctx, f, caller, ref); code(err) != kit.CodeCatalogVersionInvalid {
		t.Fatalf("unusable version must be CATALOG_VERSION_INVALID, got %v", err)
	}
	f = &fakeChecker{res: &Result{Found: true, Usable: true}}
	if err := Require(ctx, f, caller, ref); err != nil || f.last != [2]string{oid, vid} {
		t.Fatalf("usable version must pass with parsed ids, got err=%v last=%v", err, f.last)
	}
}
