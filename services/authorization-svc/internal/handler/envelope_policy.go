package handler

import (
	"net/http"

	svcenvelope "zoiko.io/authorization-svc/internal/envelope"
)

// AuthorizePath is the evaluation endpoint — the route this service exists
// for, and one of four POSTs here that answer a question instead of changing
// something.
const AuthorizePath = "/v1/authorize"

// evaluatePaths is every POST on this service that changes no state.
//
// A set rather than a chain of == comparisons because this list has to be
// exhaustive to be correct: a question-answering POST missing from it is
// classified as a material write, which under write-strict means its callers
// are refused with 401 for want of an Idempotency-Key on a question. That is
// precisely the outage tracker 82i recorded, and it went undiagnosed across
// three passes. Adding a route to RegisterRoutes puts the question "does this
// change anything" in front of whoever adds it; this map is where the answer
// goes.
var evaluatePaths = map[string]bool{
	AuthorizePath: true,

	// The three §8.3 inbound APIs added alongside the evaluation engine. All
	// three are pre-checks — "may this be granted", "which entities is this
	// principal in scope for", "is this authority borrowed" — and none of them
	// writes anything at all, not even the decision artifact /v1/authorize
	// writes. See internal/handler/validation.go.
	EntityScopeValidatePath:     true,
	SoDValidatePath:             true,
	DelegatedAccessEvaluatePath: true,
}

// EnvelopePolicy is this service's §4 policy: the generated ServicePolicy plus
// the single per-service override contract.go cannot carry, since rollout.sh
// regenerates that file and MaterialWrite is a closure.
//
// It lives beside RegisterRoutes because the thing it encodes — which of this
// service's routes change state — is a property of the route table.
func EnvelopePolicy() svcenvelope.Policy {
	p := svcenvelope.ServicePolicy()
	p.MaterialWrite = MaterialWrite
	return p
}

// MaterialWrite classifies a request as a material state change, which decides
// both the envelope's RequiredOnWrite fields and, under write-strict, whether a
// violation is refused or admitted-and-reported.
//
// None of the four POSTs in evaluatePaths is one. The three validation routes
// write literally nothing; /v1/authorize is the policy decision point: it answers
// "may this principal do this action" and returns the answer. It appends a row
// to access_decision_log, but that row is the audit record OF the question, not
// business state — two identical questions must produce two log entries, which
// is the opposite of what an idempotency key is for. This is exactly the case
// Policy.MaterialWrite's doc comment names: "Override where a service exposes a
// POST that changes nothing — a search or evaluate endpoint."
//
// Two consequences, both intended:
//
//   - Under write-strict (the default), a violating /v1/authorize call is
//     admitted, logged by the reporter and marked X-Envelope-Contract: violated
//     on the response, instead of refused with 401. That is what unblocks the
//     ~86 of 111 callers that send no envelope (tracker 82i). It is a migration
//     state, not a resting one: the reporter output is the list of callers still
//     to migrate, and every /v1/admin/* write stays strictly enforced, so the
//     relaxation is scoped to the one route that needs it.
//   - Under strict, /v1/authorize is still refused when it violates — enforced()
//     ignores this classification in that mode — but it is no longer asked for
//     Idempotency-Key or X-Legal-Entity-Id, which it has no state to protect and
//     already takes from the body. That is the correct end state for an evaluate
//     endpoint, so this override is not a hole that strict mode has to undo.
//
// A previous pass concluded from the violation list alone that this override
// "does not work", reasoning that it drops only idempotency_key and
// legal_entity_id while four unconditional fields remain. That inference missed
// that Middleware gates refusal on enforced(mode, policy.materialWrite(r)):
// under write-strict a non-write is not enforced at all, whatever Validate
// found. Measured on the wire rather than inferred this time — see progress.md.
func MaterialWrite(r *http.Request) bool {
	if r.Method == http.MethodPost && evaluatePaths[r.URL.Path] {
		return false
	}
	// Mirrors the envelope package's unexported defaultMaterialWrite: safe
	// methods only. PUT and DELETE are idempotent at the HTTP level but not at
	// the accounting level, so they count as writes.
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
