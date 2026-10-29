package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/telemetry"
)

// capture installs an in-memory exporter behind the same filter the OTLP
// exporter uses, and restores the global provider afterwards.
func capture(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(telemetry.Filter(mem)))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev); _ = tp.Shutdown(context.Background()) })
	return mem
}

// The filter is the last line of defence (Doc 08 App. B "_prohibited"):
// GenAI content keys, unknown keys, events and status text never export.
func TestFilterDropsContent(t *testing.T) {
	mem := capture(t)
	_, span := telemetry.Tracer().Start(context.Background(), "x")
	span.SetAttributes(
		attribute.String("gen_ai.input.messages", "my payroll id is 12345"),
		attribute.String("gen_ai.output.messages", "answer text"),
		attribute.String("gen_ai.system_instructions", "system prompt"),
		attribute.String("raw_prompt", "hello"),
		attribute.String("suivo.outcome", "ANSWERED"),
	)
	span.RecordError(errors.New("upstream said: my payroll id is 12345"))
	span.SetStatus(codes.Error, "upstream said: my payroll id is 12345")
	span.End()

	got := mem.GetSpans()
	if len(got) != 1 {
		t.Fatalf("spans: %d", len(got))
	}
	s := got[0]
	if len(s.Attributes) != 1 || s.Attributes[0].Key != "suivo.outcome" {
		t.Fatalf("only allowlisted keys may export, got %v", s.Attributes)
	}
	if len(s.Events) != 0 || s.Status.Description != "" || s.Status.Code != codes.Error {
		t.Fatalf("events/status text must be dropped: %+v %+v", s.Events, s.Status)
	}
}

// CT-049 / OPS-023: a full turn produces turn and model spans with outcome,
// release and usage, and nothing the visitor typed appears in any span.
func TestTurnSpansCarryNoMessageText(t *testing.T) {
	mem := capture(t)
	env, err := fixtureenv.NewPublicKB(context.Background(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	msg := "What does Legal and Contracts do? My email is jane.roe@example.com"
	r := env.Pipeline.Answer(context.Background(), orchestrator.Input{RequestID: "req_otel", Message: msg, Locale: "en-US"})

	names := map[string]bool{}
	for _, s := range mem.GetSpans() {
		names[s.Name] = true
		for _, kv := range s.Attributes {
			v := strings.ToLower(kv.Value.Emit())
			for _, word := range []string{"jane", "example.com", "legal and contracts do", "email"} {
				if strings.Contains(v, word) {
					t.Errorf("span %q attribute %s leaks visitor text: %q", s.Name, kv.Key, v)
				}
			}
		}
	}
	if !names["public_ai.turn"] {
		t.Fatalf("no turn span; got %v", names)
	}
	turn := find(mem.GetSpans(), "public_ai.turn")
	if attr(turn, "suivo.outcome") != string(r.Status) || attr(turn, "suivo.release.kb") == "" {
		t.Fatalf("turn span attributes: %v", turn.Attributes)
	}
	if r.Status == "ANSWERED" && attr(turn, "suivo.retrieval.count") == "0" {
		t.Fatal("an answered turn records its retrieval count")
	}
}

func find(spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	return tracetest.SpanStub{}
}

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// Without an endpoint, Setup exports nothing.
func TestSetupOffWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	if telemetry.Enabled() {
		t.Fatal("must be off without an endpoint")
	}
	shutdown, err := telemetry.Setup(context.Background(), "test")
	if err != nil || shutdown(context.Background()) != nil {
		t.Fatal(err)
	}
}

// The real OTLP/HTTP exporter, pointed at a local receiver: spans arrive and
// the wire payload holds no visitor text.
func TestOTLPExportOverTheWire(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", recv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
	prev := otel.GetTracerProvider()
	shutdown, err := telemetry.Setup(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer otel.SetTracerProvider(prev)

	env, err := fixtureenv.NewPublicKB(context.Background(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	env.Pipeline.Answer(context.Background(), orchestrator.Input{RequestID: "req_wire", Message: "What does Legal and Contracts do? call me on 07700 900123", Locale: "en-US"})
	if err := shutdown(context.Background()); err != nil { // flushes the batch
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	all := bytes.Join(bodies, nil)
	if !bytes.Contains(all, []byte("public_ai.turn")) {
		t.Fatalf("no turn span reached the receiver (%d requests)", len(bodies))
	}
	for _, leak := range []string{"07700", "Legal and Contracts do", "call me"} {
		if bytes.Contains(all, []byte(leak)) {
			t.Fatalf("exported payload contains visitor text %q", leak)
		}
	}
}
