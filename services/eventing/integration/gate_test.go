package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubRegistry struct {
	schemas map[string]string
	down    atomic.Bool
	calls   atomic.Int32
}

func (s *stubRegistry) Schema(_ context.Context, name string, _ int) (*Schema, error) {
	s.calls.Add(1)
	if s.down.Load() {
		return nil, fmt.Errorf("%w: down", ErrRegistryUnavailable)
	}
	raw, ok := s.schemas[name]
	if !ok {
		return nil, Wrap(CodeSchemaMismatch, "none registered", ErrNoContract)
	}
	return &Schema{EventName: name, Version: 4, JSONSchema: []byte(raw)}, nil
}

const gateSchema = `{"type":"object","required":["id","n"],"properties":{"id":{"type":"string"},"n":{"type":"integer"}}}`

func event(data string) []byte {
	return []byte(`{"type":"com.zoikosuite.sales.order.placed","data":` + data + `}`)
}

func TestGateAcceptsConformingData(t *testing.T) {
	g := &SchemaGate{Registry: &stubRegistry{schemas: map[string]string{"order.placed": gateSchema}}}
	if err := g.Check(context.Background(), "com.zoikosuite.sales.order.placed", event(`{"id":"a","n":2}`)); err != nil {
		t.Fatal(err)
	}
}

func TestGateRejectsNonConformingNamingTheContract(t *testing.T) {
	g := &SchemaGate{Registry: &stubRegistry{schemas: map[string]string{"order.placed": gateSchema}}}
	err := g.Check(context.Background(), "com.zoikosuite.sales.order.placed", event(`{"id":"a"}`))
	e, ok := As(err)
	if !ok || e.Code != CodeSchemaMismatch || !strings.Contains(e.Detail, "order.placed v4") {
		t.Fatalf("got %v", err)
	}
	if errors.Is(err, ErrRegistryUnavailable) {
		t.Fatal("a mismatch is not an outage")
	}
}

func TestGateDistinguishesOutageFromMismatch(t *testing.T) {
	reg := &stubRegistry{schemas: map[string]string{"order.placed": gateSchema}}
	reg.down.Store(true)
	g := &SchemaGate{Registry: reg}
	if err := g.Check(context.Background(), "com.zoikosuite.sales.order.placed", event(`{}`)); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("outage must surface as unavailable, never as a verdict: %v", err)
	}
}

func TestGateUnregisteredAndMalformedEvents(t *testing.T) {
	g := &SchemaGate{Registry: &stubRegistry{schemas: map[string]string{}}}
	if err := g.Check(context.Background(), "com.zoikosuite.sales.order.placed", event(`{}`)); err == nil {
		t.Fatal("unregistered contract must be refused by default")
	}
	g.AllowUnregistered = true
	if err := g.Check(context.Background(), "com.zoikosuite.sales.order.placed", event(`{}`)); err != nil {
		t.Fatalf("explicitly allowed: %v", err)
	}
	g2 := &SchemaGate{Registry: &stubRegistry{schemas: map[string]string{"order.placed": gateSchema}}}
	for name, body := range map[string][]byte{"not json": []byte(`{`), "no data": []byte(`{"type":"x"}`)} {
		if err := g2.Check(context.Background(), "com.zoikosuite.sales.order.placed", body); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := (&SchemaGate{}).Check(context.Background(), "x.y", event(`{}`)); err == nil {
		t.Error("gate without a registry must fail, not pass")
	}
	if err := g2.Check(context.Background(), "", event(`{}`)); err == nil {
		t.Error("event with no name accepted")
	}
}

func TestGateCacheIsBoundedAndCustomResolveWorks(t *testing.T) {
	reg := &stubRegistry{schemas: map[string]string{}}
	g := &SchemaGate{Registry: reg, AllowUnregistered: true, TTL: time.Hour,
		Resolve: func(eventType string, _ []byte) string { return eventType }}
	for i := 0; i < maxCacheEntries+50; i++ {
		_ = g.Check(context.Background(), fmt.Sprintf("a.b%d", i), event(`{}`))
	}
	g.mu.Lock()
	n := len(g.cache)
	g.mu.Unlock()
	if n > maxCacheEntries {
		t.Fatalf("cache grew to %d, limit %d", n, maxCacheEntries)
	}
}
