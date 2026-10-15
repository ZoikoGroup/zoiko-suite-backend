package envelope

import (
	"net/http"
	"os"
	"strings"

	"zoiko.io/jurisdiction-rules-svc/internal/problem"
)

// Mode controls what Middleware does with a request that fails §4.
type Mode string

const (
	// ModeStrict refuses the request. This is the doctrinal behaviour: §4 calls
	// these fields mandatory and the platform is fail-closed everywhere else.
	ModeStrict Mode = "strict"

	// ModeWriteStrict refuses material state changes and admits reads. It is the
	// default because it draws the line where the invariants actually bite —
	// INV-08 replay protection and INV-10 evidence-before-completion are
	// properties of writes — while a GET that omits request_id is a traceability
	// gap, not a correctness one. It also lets the envelope be adopted without
	// simultaneously breaking every existing read path in the console.
	ModeWriteStrict Mode = "write-strict"

	// ModeObserve never refuses. The envelope is still parsed, propagated and
	// reported on the response, so a service can be deployed and its callers
	// migrated before enforcement is turned on. Nothing is gated in this mode:
	// it is a migration state, not a resting state.
	ModeObserve Mode = "observe"
)

// EnvVarMode is the per-service override. Deployments set it to "strict" once
// every caller of that service sends a full envelope.
const EnvVarMode = "ZS_ENVELOPE_ENFORCEMENT"

// ResolveMode reads EnvVarMode, defaulting to ModeWriteStrict. An unrecognised
// value falls back to the default rather than to observe: a typo in a
// deployment variable must not silently disable a control.
func ResolveMode() Mode {
	switch Mode(strings.ToLower(strings.TrimSpace(os.Getenv(EnvVarMode)))) {
	case ModeStrict:
		return ModeStrict
	case ModeObserve:
		return ModeObserve
	default:
		return ModeWriteStrict
	}
}

// Reporter receives envelopes that failed validation but were admitted anyway,
// so observe-mode adoption is measurable instead of invisible. Wire it to the
// service's zap logger. May be nil.
type Reporter func(r *http.Request, e Envelope, err *ValidationError)

// Middleware parses the §4 envelope, validates it against policy, and places it
// on the request context.
//
// It runs before authorization rather than after. A request that cannot say who
// is acting, for which tenant, under which correlation, is not a request that
// should reach an authorization decision — and letting it through would put
// un-attributable entries in authorization-svc's append-only decision log.
func Middleware(policy Policy, report Reporter) func(http.Handler) http.Handler {
	return MiddlewareWithMode(policy, ResolveMode(), report)
}

// MiddlewareWithMode is Middleware with the mode supplied directly, for tests
// and for services that pin their own enforcement.
func MiddlewareWithMode(policy Policy, mode Mode, report Reporter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			e := Parse(r)

			// Echoed before any refusal, so a caller debugging a 400 can still
			// find the request in the logs it is being refused by.
			if e.RequestID != "" {
				w.Header().Set(HeaderRequestID, e.RequestID)
			}
			if e.CorrelationID != "" {
				w.Header().Set(HeaderCorrelationID, e.CorrelationID)
			}

			if err := policy.Validate(e, r); err != nil {
				if enforced(mode, policy.materialWrite(r)) {
					writeViolation(w, err)
					return
				}
				if report != nil {
					report(r, e, err)
				}
				// Named on the response as well as logged: a caller migrating
				// onto the envelope can see it is out of contract from the
				// response alone, without access to the service's logs.
				w.Header().Set("X-Envelope-Contract", "violated")
			}

			next.ServeHTTP(w, r.WithContext(WithEnvelope(r.Context(), e)))
		})
	}
}

func enforced(mode Mode, isWrite bool) bool {
	switch mode {
	case ModeStrict:
		return true
	case ModeWriteStrict:
		return isWrite
	default:
		return false
	}
}

// writeViolation renders the refusal as an RFC 9457 problem (API standard §12),
// carrying each unmet §4 obligation as an `errors[]` entry located by JSON
// Pointer.
//
// The per-field detail is deliberately not folded into a single sentence. The
// Next.js console's readErrorDetail concatenates error/field/message/detail
// into one string, and schema-registry-svc already showed what that costs when
// the structure is the point: a caller cannot tell which of five headers it is
// missing from a folded sentence. Keeping them structured lets the console
// list them.
func writeViolation(w http.ResponseWriter, err *ValidationError) {
	w.Header().Set("X-Envelope-Contract", "violated")
	p := problem.New(StatusFor(err), "envelope_incomplete", err.Error())
	for _, v := range err.Violations {
		p.Field("#/"+v.Field, "violation", v.Header+": "+v.Reason)
	}
	if err.Service != "" {
		p.With("service", err.Service)
	}
	problem.Write(w, p)
}
