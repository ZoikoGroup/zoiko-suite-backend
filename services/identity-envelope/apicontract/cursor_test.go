package apicontract

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func testSigner(t *testing.T) *CursorSigner {
	t.Helper()
	s, err := NewCursorSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCursorSignVerifyRoundTrip(t *testing.T) {
	s := testSigner(t)
	in := Cursor{TenantID: "t1", Route: "/v1/invoices", Filter: "state=open", Sort: "-created_at,id", Offset: 100, LastID: "inv_9"}
	token, err := s.Sign(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Verify(token, "t1", "/v1/invoices", "state=open", "-created_at,id")
	if err != nil {
		t.Fatal(err)
	}
	if out.Offset != 100 || out.LastID != "inv_9" {
		t.Fatalf("cursor = %+v", out)
	}
}

func TestCursorRejectsTamperedPayloadAndSignature(t *testing.T) {
	s := testSigner(t)
	token, err := s.Sign(Cursor{TenantID: "t1", Route: "/v1/invoices"})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	forged := parts[0] + "A." + parts[1]
	if _, err := s.Verify(forged, "t1", "/v1/invoices", "", ""); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("tampered payload err = %v", err)
	}
	if _, err := s.Verify(parts[0]+".AAAA", "t1", "/v1/invoices", "", ""); !errors.Is(err, ErrCursorInvalid) {
		t.Fatalf("tampered signature err = %v", err)
	}
	for _, bad := range []string{"", "one", "a.b.c", "!!.??"} {
		if _, err := s.Verify(bad, "t1", "/v1/invoices", "", ""); !errors.Is(err, ErrCursorInvalid) {
			t.Fatalf("verify %q err = %v", bad, err)
		}
	}
}

func TestCursorRejectsOutOfScope(t *testing.T) {
	s := testSigner(t)
	token, err := s.Sign(Cursor{TenantID: "t1", Route: "/v1/invoices", Filter: "state=open", Sort: "id"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ tenant, route, filter, sort string }{
		{"t2", "/v1/invoices", "state=open", "id"},
		{"t1", "/v1/bills", "state=open", "id"},
		{"t1", "/v1/invoices", "state=paid", "id"},
		{"t1", "/v1/invoices", "state=open", "-id"},
	}
	for _, c := range cases {
		if _, err := s.Verify(token, c.tenant, c.route, c.filter, c.sort); !errors.Is(err, ErrCursorScope) {
			t.Fatalf("verify %+v err = %v", c, err)
		}
	}
}

func TestCursorSignerRejectsShortKey(t *testing.T) {
	if _, err := NewCursorSigner([]byte("short")); err == nil {
		t.Fatal("accepted a short signing key")
	}
}

func TestCursorRequiresScope(t *testing.T) {
	s := testSigner(t)
	if _, err := s.Sign(Cursor{Route: "/v1/invoices"}); err == nil {
		t.Fatal("signed a cursor without a tenant")
	}
	if _, err := s.Sign(Cursor{TenantID: "t1"}); err == nil {
		t.Fatal("signed a cursor without a route")
	}
}

func TestParsePageDefaults(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/invoices", nil)
	p, prob := ParsePage(r, []string{"state", "customer_id"}, []string{"created_at", "id"}, "-created_at")
	if prob != nil {
		t.Fatalf("problem = %v", prob)
	}
	if p.PageSize != DefaultPageSize {
		t.Fatalf("page size = %d", p.PageSize)
	}
	if p.SortKey() != "-created_at,id" {
		t.Fatalf("sort = %q", p.SortKey())
	}
}

func TestParsePageRejectsUnknownFilterAndBadSizeAndSort(t *testing.T) {
	cases := map[string]string{
		"unknown-filter": "/v1/invoices?filter%5Bsecret%5D=x",
		"bad-size-high":  "/v1/invoices?page_size=201",
		"bad-size-zero":  "/v1/invoices?page_size=0",
		"bad-size-text":  "/v1/invoices?page_size=ten",
		"bad-sort":       "/v1/invoices?sort=password",
		"bad-filter-op":  "/v1/invoices?filter%5Bstate%5D%5Blike%5D=x",
	}
	for name, target := range cases {
		p, prob := ParsePage(httptest.NewRequest("GET", target, nil), []string{"state"}, []string{"created_at", "id"}, "id")
		if prob == nil {
			t.Fatalf("%s: accepted %+v", name, p)
		}
		switch name {
		case "unknown-filter":
			if prob.Code != "unknown_filter" {
				t.Fatalf("%s: code = %s", name, prob.Code)
			}
		case "bad-size-high", "bad-size-zero", "bad-size-text":
			if prob.Code != "invalid_page_size" {
				t.Fatalf("%s: code = %s", name, prob.Code)
			}
		case "bad-sort", "bad-filter-op":
			if prob.Code != "invalid_sort" && prob.Code != "invalid_filter" {
				t.Fatalf("%s: code = %s", name, prob.Code)
			}
		}
	}
}

func TestParsePageFiltersAndCursor(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/invoices?page_size=25&filter%5Bstate%5D=open&filter%5Btotal%5D%5Bgte%5D=100&sort=-created_at&cursor=abc", nil)
	p, prob := ParsePage(r, []string{"state", "total"}, []string{"created_at", "id"}, "id")
	if prob != nil {
		t.Fatalf("problem = %v", prob)
	}
	if p.PageSize != 25 || p.Cursor != "abc" {
		t.Fatalf("page = %+v", p)
	}
	if p.FilterKey() != "state=open&total[gte]=100" {
		t.Fatalf("filter key = %q", p.FilterKey())
	}
	if p.SortKey() != "-created_at,id" {
		t.Fatalf("sort key = %q", p.SortKey())
	}
}
