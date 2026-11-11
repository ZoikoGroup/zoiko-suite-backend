package vaultref

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGoverned_AsksUnderTheTenantAsTheWorkloadAndFailsClosed(t *testing.T) {
	var path, query, tenant, principal string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query, tenant, principal = r.URL.Path, r.URL.Query().Get("secret_path"), r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "svc-configuration", 0, nil)

	ok, err := c.Governed(context.Background(), "t1", "corr", "kv/smtp")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if path != "/v1/secret-paths/status" || query != "kv/smtp" || tenant != "t1" || principal != "svc-configuration" {
		t.Fatalf("path=%s query=%s tenant=%s principal=%s", path, query, tenant, principal)
	}

	status = http.StatusNotFound
	if ok, err = c.Governed(context.Background(), "t1", "", "kv/none"); err != nil || ok {
		t.Fatalf("404 is an answer: ok=%v err=%v", ok, err)
	}
	for _, s := range []int{401, 403, 500, 503} {
		status = s
		if ok, err = c.Governed(context.Background(), "t1", "", "kv/x"); ok || err == nil {
			t.Fatalf("status %d must refuse, never accept: ok=%v err=%v", s, ok, err)
		}
	}
	srv.Close()
	if ok, err = c.Governed(context.Background(), "t1", "", "kv/x"); ok || err == nil {
		t.Fatal("an unreachable vault must refuse")
	}
}
