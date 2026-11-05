package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/siem"
)

type recordingRevokedPublisher struct{ ids []string }

func (r *recordingRevokedPublisher) PublishSessionInvalidated(_ context.Context, id, _ string, _ domain.InvalidationReason, _ string) error {
	r.ids = append(r.ids, id)
	return nil
}

// TestSessionCap_EvictionStreamsSIEM: a cap eviction is published and reaches
// siem-integration-svc as identity.session.revoked.
func TestSessionCap_EvictionStreamsSIEM(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/siem/exporters" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"exp-1","status":"ACTIVE"}]}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.Header.Get("X-Tenant-ID")+" "+string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := siem.New(srv.URL, "identity-context-svc", zap.NewNop())
	pub := &recordingRevokedPublisher{}
	hook := capRevocationHook(pub, client, zap.NewNop())

	hook(context.Background(), "sc-old", "p-1", "tenant-1", domain.InvalidationReasonSessionConcurrencyCap, "corr-1")
	client.Close() // drains the queue

	if len(pub.ids) != 1 || pub.ids[0] != "sc-old" {
		t.Fatalf("revocation not published: %v", pub.ids)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("SIEM received %d events, want 1", len(bodies))
	}
	for _, want := range []string{"identity.session.revoked", "tenant-1", "sc-old", string(domain.InvalidationReasonSessionConcurrencyCap)} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("SIEM event lacks %q: %s", want, bodies[0])
		}
	}
}
