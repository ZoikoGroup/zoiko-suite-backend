package vaultsecrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUse_AsksAsTheWorkloadUnderTheTenantAndDecodesTheCredential(t *testing.T) {
	var hdr http.Header
	var body map[string]string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if r.URL.Path != "/v1/secrets/use" || r.Method != http.MethodPost {
			t.Errorf("wrong route %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"credential_base64":"` + base64.StdEncoding.EncodeToString([]byte("s3cret-value")) + `"}`))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "notification-svc", "scope-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Use(context.Background(), "kv/smtp")
	if err != nil || string(got) != "s3cret-value" {
		t.Fatalf("got %q err %v", got, err)
	}
	if body["secret_path"] != "kv/smtp" || body["requested_by_principal_id"] != "notification-svc" || body["request_id"] == "" {
		t.Fatalf("body %v", body)
	}
	if hdr.Get("X-Principal-Id") != "notification-svc" || hdr.Get("X-Tenant-Id") != "scope-1" || hdr.Get("X-Purpose-Context") == "" ||
		hdr.Get("X-Source-Channel") != "system" || hdr.Get("Idempotency-Key") == "" {
		t.Fatalf("the vault enforces the canonical envelope: %v", hdr)
	}
	for _, s := range []int{401, 403, 404, 500, 503} {
		status = s
		if _, err := c.Use(context.Background(), "kv/smtp"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("status %d must be a failure, got %v", s, err)
		}
	}
	srv.Close()
	if _, err := c.Use(context.Background(), "kv/smtp"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unreachable vault is a failure: %v", err)
	}
	if _, err := NewClient("not a url", "p", "t", nil); err == nil {
		t.Fatal("a relative base URL is refused")
	}
	if _, err := NewClient(srv.URL, "", "t", nil); err == nil {
		t.Fatal("a principal is required")
	}
}

type scriptedFetcher struct {
	vals map[string]string
	err  map[string]error
}

func (s *scriptedFetcher) Use(_ context.Context, path string) ([]byte, error) {
	if e := s.err[path]; e != nil {
		return nil, e
	}
	return []byte(s.vals[path]), nil
}

func TestRefresher_LoadFailsClosedAndRefreshKeepsTheValueInUse(t *testing.T) {
	f := &scriptedFetcher{vals: map[string]string{"kv/a": "one"}, err: map[string]error{}}
	var applied []string
	r := NewRefresher(f, []Secret{{Name: "a", Path: "kv/a", Apply: func(v []byte) error { applied = append(applied, string(v)); return nil }}}, nil)

	f.err["kv/a"] = ErrUnavailable
	if err := r.Load(context.Background()); !errors.Is(err, ErrUnavailable) || len(applied) != 0 {
		t.Fatalf("start-up must fail when a secret cannot be obtained: %v %v", err, applied)
	}
	delete(f.err, "kv/a")
	if err := r.Load(context.Background()); err != nil || len(applied) != 1 || applied[0] != "one" {
		t.Fatalf("%v %v", err, applied)
	}

	if n := r.Refresh(context.Background()); n != 0 || len(applied) != 1 {
		t.Fatal("an unchanged secret is not re-applied")
	}
	f.vals["kv/a"] = "two"
	if n := r.Refresh(context.Background()); n != 1 || applied[len(applied)-1] != "two" {
		t.Fatalf("a rotation is applied: %d %v", n, applied)
	}
	f.err["kv/a"] = ErrUnavailable
	f.vals["kv/a"] = "three"
	if n := r.Refresh(context.Background()); n != 0 || applied[len(applied)-1] != "two" {
		t.Fatalf("an outage keeps the value in use: %d %v", n, applied)
	}
}

func TestRefresher_ARefusedRotationKeepsTheOldValueAndIsRetried(t *testing.T) {
	f := &scriptedFetcher{vals: map[string]string{"kv/w": "good"}, err: map[string]error{}}
	var current string
	refuse := false
	r := NewRefresher(f, []Secret{{Name: "w", Path: "kv/w", Apply: func(v []byte) error {
		if refuse {
			return errors.New("malformed")
		}
		current = string(v)
		return nil
	}}}, nil)
	if err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.vals["kv/w"] = "bad"
	refuse = true
	if n := r.Refresh(context.Background()); n != 0 || current != "good" {
		t.Fatalf("a refused value is not applied: %d %q", n, current)
	}
	refuse = false
	if n := r.Refresh(context.Background()); n != 1 || current != "bad" {
		t.Fatalf("and is retried on the next refresh: %d %q", n, current)
	}
}
