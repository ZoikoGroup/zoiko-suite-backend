package partycontacts

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerifiedEmails_AsksUnderTheTenantAndDropsAnythingNotVerifiedInForce(t *testing.T) {
	var path, query, tenant, principal string
	status := http.StatusOK
	body := `{"contact_points":[
	  {"contact_point_id":"cp-old","channel":"EMAIL","value":"old@ex.com","purpose":"GENERAL","status":"VERIFIED","valid_from":"2026-01-01T00:00:00Z","verified_at":"2026-02-01T00:00:00Z"},
	  {"contact_point_id":"cp-new","channel":"email","value":"new@ex.com","purpose":"GENERAL","status":"VERIFIED","valid_from":"2026-01-01T00:00:00Z","verified_at":"2026-06-01T00:00:00Z"},
	  {"contact_point_id":"cp-prop","channel":"EMAIL","value":"p@ex.com","purpose":"GENERAL","status":"PROPOSED","valid_from":"2026-01-01T00:00:00Z"},
	  {"contact_point_id":"cp-ended","channel":"EMAIL","value":"e@ex.com","purpose":"GENERAL","status":"VERIFIED","valid_from":"2026-01-01T00:00:00Z","valid_to":"2026-03-01T00:00:00Z","verified_at":"2026-02-01T00:00:00Z"},
	  {"contact_point_id":"cp-future","channel":"EMAIL","value":"f@ex.com","purpose":"GENERAL","status":"VERIFIED","valid_from":"2027-01-01T00:00:00Z","verified_at":"2026-02-01T00:00:00Z"},
	  {"contact_point_id":"cp-sms","channel":"PHONE","value":"+4400","purpose":"GENERAL","status":"VERIFIED","valid_from":"2026-01-01T00:00:00Z"},
	  {"contact_point_id":"cp-empty","channel":"EMAIL","value":"","purpose":"GENERAL","status":"VERIFIED","valid_from":"2026-01-01T00:00:00Z"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query, tenant, principal = r.URL.Path, r.URL.Query().Get("status")+"|"+r.URL.Query().Get("channel"), r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "notification-svc", 0, nil)
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	got, err := c.VerifiedEmails(context.Background(), "t1", "corr", "party-9", at)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/parties/party-9/contact-points" || query != "VERIFIED|EMAIL" || tenant != "t1" || principal != "notification-svc" {
		t.Fatalf("path=%s query=%s tenant=%s principal=%s", path, query, tenant, principal)
	}
	if len(got) != 2 || got[0].ContactPointID != "cp-new" || got[1].ContactPointID != "cp-old" {
		t.Fatalf("only verified email in force at the instant, newest verification first: %+v", got)
	}

	status = http.StatusNotFound
	if _, err := c.VerifiedEmails(context.Background(), "t1", "", "nope", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a party of another tenant, or none, is not found: %v", err)
	}
	for _, s := range []int{401, 403, 500, 503} {
		status = s
		if _, err := c.VerifiedEmails(context.Background(), "t1", "", "p", at); !errors.Is(err, ErrUnavailable) {
			t.Errorf("status %d is not an answer about the party: %v", s, err)
		}
	}
	srv.Close()
	if _, err := c.VerifiedEmails(context.Background(), "t1", "", "p", at); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("an unreachable registry is unavailable: %v", err)
	}
}
