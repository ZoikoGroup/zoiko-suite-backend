package handler_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	kmscrypto "zoiko.io/key-management-svc/internal/crypto"
	"zoiko.io/key-management-svc/internal/handler"
	"zoiko.io/key-management-svc/internal/idempotency"
	"zoiko.io/key-management-svc/internal/siem"
	"zoiko.io/key-management-svc/internal/store"
)

// recordingIdem is an idempotency store that records every endpoint it is
// asked to claim and every response body it is asked to keep.
type recordingIdem struct {
	*idempotency.MemoryStore
	mu        sync.Mutex
	endpoints []string
	bodies    [][]byte
}

func (s *recordingIdem) Claim(ctx context.Context, tenantID, endpoint, key, fingerprint string) (*idempotency.Record, error) {
	s.mu.Lock()
	s.endpoints = append(s.endpoints, endpoint)
	s.mu.Unlock()
	return s.MemoryStore.Claim(ctx, tenantID, endpoint, key, fingerprint)
}

func (s *recordingIdem) Complete(ctx context.Context, tenantID, endpoint, key string, status int, body []byte) error {
	s.mu.Lock()
	s.bodies = append(s.bodies, append([]byte(nil), body...))
	s.mu.Unlock()
	return s.MemoryStore.Complete(ctx, tenantID, endpoint, key, status, body)
}

// The replay store keeps response bodies, and /v1/detokenize answers with a
// raw bank-account number, so the vault routes must never pass through it.
// A /v1/keys write is the control: it proves the store is wired at all.
func TestVault_IdempotencyStoreNeverSeesVaultTraffic(t *testing.T) {
	s := store.NewMemoryStore()
	cipher, err := kmscrypto.New(vaultKey)
	if err != nil {
		t.Fatal(err)
	}
	idem := &recordingIdem{MemoryStore: idempotency.NewMemoryStore()}
	sink := newFakeSIEM(t)
	sc := siem.New(sink.srv.URL, "key-management-svc", zap.NewNop())
	e := &vaultEnv{
		router: handler.NewRouter(handler.New(s, sc, &recordingAuthz{deny: map[string]error{}}, zap.NewNop()).
			WithVault(s, cipher).WithIdempotency(idem)),
		siem: sc, sink: sink,
	}
	defer e.drain()

	tenant := uuid.NewString()
	tok := tokenize(t, e, tenant, rawIBAN)
	if w := detokenize(e, tenant, "auditor", tok); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), rawIBAN) {
		t.Fatalf("detokenize: %d %s", w.Code, w.Body.String())
	}
	idem.mu.Lock()
	vaultClaims := len(idem.endpoints)
	for _, b := range idem.bodies {
		if bytes.Contains(b, []byte(rawIBAN)) {
			idem.mu.Unlock()
			t.Fatal("a detokenized value was written to the idempotency store")
		}
	}
	idem.mu.Unlock()
	if vaultClaims != 0 {
		t.Fatalf("vault routes claimed idempotency keys: %v", idem.endpoints)
	}

	post(e.router, "/v1/keys", map[string]string{"legal_entity_id": "le-1"},
		func(r *http.Request) { clientHeaders(r, tenant, "svc-principal", "le-1") })
	idem.mu.Lock()
	defer idem.mu.Unlock()
	if len(idem.endpoints) == 0 {
		t.Fatal("control: a /v1/keys write did not reach the idempotency store, so the check above proves nothing")
	}
}
