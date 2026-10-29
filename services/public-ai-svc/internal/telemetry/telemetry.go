// Package telemetry exports privacy-safe OpenTelemetry traces (Doc 08 §14,
// App. B). Export is off unless OTEL_EXPORTER_OTLP_ENDPOINT (or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) is set; the standard OTEL_* variables
// configure the OTLP/HTTP exporter and the sampler.
//
// Content never leaves the process: the exporter is wrapped so that only
// allowlisted attribute keys are sent, span events (exception messages) and
// status descriptions are dropped, and GenAI input/output attributes are
// impossible to export even if a library adds them.
package telemetry

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const scope = "zoiko.io/public-ai-svc"

// Allowed is the complete set of span attribute keys that may be exported.
// Everything else is dropped at the exporter. Keys follow Doc 08 App. B.
var Allowed = map[attribute.Key]bool{
	"http.request.method":        true,
	"http.route":                 true,
	"http.response.status_code":  true,
	"request_id":                 true,
	"error.type":                 true,
	"suivo.outcome":              true,
	"suivo.reason":               true,
	"suivo.intent":               true,
	"suivo.release.kb":           true,
	"suivo.release.policy":       true,
	"suivo.retrieval.mode":       true,
	"suivo.retrieval.count":      true,
	"suivo.retrieval.chunk_ids":  true, // public chunk ids only
	"suivo.claim.verdict":        true,
	"suivo.citation.count":       true,
	"suivo.answer_class":         true,
	"suivo.draft_content":        true,
	"gen_ai.operation.name":      true,
	"gen_ai.request.model":       true,
	"gen_ai.usage.input_tokens":  true,
	"gen_ai.usage.output_tokens": true,
	"suivo.model.attempts":       true,
}

// Tracer is the service tracer. It is a no-op until Setup installs a provider.
func Tracer() trace.Tracer { return otel.Tracer(scope) }

// Enabled reports whether an OTLP endpoint is configured.
func Enabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// Setup installs the tracer provider and the W3C trace-context propagator.
// Without an endpoint it installs only the propagator and returns a no-op
// shutdown, so nothing is exported.
func Setup(ctx context.Context, environment string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if !Enabled() {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	name := os.Getenv("OTEL_SERVICE_NAME")
	if name == "" {
		name = "public-ai-svc"
	}
	res := resource.NewSchemaless(
		attribute.String("service.name", name),
		attribute.String("deployment.environment.name", environment),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(Filter(exp)),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Filter wraps an exporter so only allowlisted, content-free data is sent.
func Filter(next sdktrace.SpanExporter) sdktrace.SpanExporter { return filter{next} }

type filter struct{ next sdktrace.SpanExporter }

func (f filter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = safeSpan{s}
	}
	return f.next.ExportSpans(ctx, out)
}

func (f filter) Shutdown(ctx context.Context) error { return f.next.Shutdown(ctx) }

// safeSpan is a read-only view with disallowed data removed.
type safeSpan struct{ sdktrace.ReadOnlySpan }

func (s safeSpan) Attributes() []attribute.KeyValue {
	var kept []attribute.KeyValue
	for _, kv := range s.ReadOnlySpan.Attributes() {
		if Allowed[kv.Key] {
			kept = append(kept, kv)
		}
	}
	return kept
}

func (s safeSpan) Events() []sdktrace.Event { return nil }

func (s safeSpan) Status() sdktrace.Status {
	return sdktrace.Status{Code: s.ReadOnlySpan.Status().Code}
}

func (s safeSpan) DroppedAttributes() int { return 0 }

func (s safeSpan) Links() []sdktrace.Link {
	links := s.ReadOnlySpan.Links()
	for i := range links {
		links[i].Attributes = nil
	}
	return links
}

// Fail marks a span as failed with a fixed error.type (never err.Error()).
func Fail(span trace.Span, errorType string) {
	span.SetAttributes(attribute.String("error.type", errorType))
	span.SetStatus(codes.Error, "")
}
