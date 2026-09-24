package envelope

import (
	"log/slog"
	"net/http"
)

// This file deliberately depends only on log/slog, not on zap. The package is
// vendored into 87 separate modules; a zap import would make the vendored copy
// fail to build in any service that does not already require zap, and would tie
// the contract's release cadence to a logging library's.
//
// A service that logs with zap wires its own closure instead:
//
//	r.Use(envelope.Middleware(envelope.ServicePolicy(),
//	    func(r *http.Request, e envelope.Envelope, err *envelope.ValidationError) {
//	        logger.Warn("envelope contract violated",
//	            zap.String("operation", e.Operation),
//	            zap.String("correlation_id", e.CorrelationID),
//	            zap.Strings("missing", envelope.Fields(err)))
//	    }))

// DefaultReporter is what services wire when they have no reason to do
// otherwise. It exists so the one line added to 87 main.go files needs no
// second import and no knowledge of what each service named its logger — those
// differ across the repo, and a wiring line that has to be adapted per service
// is a wiring line that will be adapted wrongly somewhere.
func DefaultReporter() Reporter { return SlogReporter(slog.Default()) }

// SlogReporter reports admitted-but-non-compliant requests to a slog.Logger.
//
// It logs the field names only — never header values, and never the request
// body. INV-15 requires telemetry that does not indiscriminately expose
// financial, personal, bank, tax or privileged content, and an envelope carries
// exactly the identifiers (external_reference, evidence_refs, purpose_context)
// that would breach it if logged verbatim.
func SlogReporter(l *slog.Logger) Reporter {
	if l == nil {
		return nil
	}
	return func(r *http.Request, e Envelope, err *ValidationError) {
		l.Warn("canonical input contract violated",
			slog.String("service", err.Service),
			slog.String("operation", e.Operation),
			slog.String("method", r.Method),
			slog.String("correlation_id", e.CorrelationID),
			slog.String("request_id", e.RequestID),
			slog.Any("missing", Fields(err)),
		)
	}
}

// Fields returns just the field names from a ValidationError, for structured
// logging and metric labels. Safe to emit: these are contract field names, not
// the caller's values.
func Fields(err *ValidationError) []string {
	if err == nil {
		return nil
	}
	out := make([]string, 0, len(err.Violations))
	for _, v := range err.Violations {
		out = append(out, v.Field)
	}
	return out
}
