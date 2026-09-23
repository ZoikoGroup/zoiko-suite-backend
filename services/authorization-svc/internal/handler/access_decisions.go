package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// AccessDecisionsPath is the collection route for the decision log.
// Referenced by MaterialWrite, which has to know it is a read.
const AccessDecisionsPath = "/v1/access-decisions"

// The two outcomes the log records, and the only two an outcome filter may
// name. A var rather than a switch in the validator so the set is one thing to
// change if a third outcome is ever recorded.
var decisionOutcomes = map[string]bool{"GRANTED": true, "DENIED": true}

// ListAccessDecisions handles GET /v1/access-decisions — read the decision
// log.
//
// ── WHY THIS ROUTE EXISTS ───────────────────────────────────────────────────
//
// Doc 03 §8.3 places two evidence obligations on this service:
//
//	every decision logged with actor, action, basis, and outcome
//	denials must be evidentially retrievable
//
// The first has held since the first migration. The second had not, and the
// reason was not the schema — it was that GET /v1/access-decisions/{id} was the
// only read. Retrieval by primary key is retrieval only for somebody who
// already holds the key, and a denial's key exists in exactly one place: the
// response body handed to the service that was refused. Auditing a denial
// therefore meant going to the CALLING service's logs to find a UUID to bring
// back to this one. The console's own lookup box said as much in its hint:
// "take it from the answer a check gave you, or from the service that was
// refused — the reference is in its logs."
//
// ── AUTHENTICATED AND TENANT-SCOPED, ON PURPOSE ─────────────────────────────
//
// This is the who-tried-to-do-what map for a whole tenant. A denial's
// decision_basis names the SoD rule that fired, so an unscoped listing would
// hand a reader the location of every segregation-of-duties tripwire on the
// platform — which is why FindAccessDecisionByID was hardened to require a
// tenant in the first place. The scope comes from the verified X-Tenant-Id and
// is passed to the store OUTSIDE the filter struct; no query parameter can
// widen it.
//
// Decisions recorded with NO tenant (the 86 callers that send no envelope yet)
// are not visible here at all, for the same reason they are not visible through
// the by-id read: they cannot be attributed to a tenant, so serving them to one
// would be a guess. That is a known consequence of the envelope migration
// rather than a property of this route, and it is recorded as such.
//
// ── QUERY PARAMETERS ────────────────────────────────────────────────────────
//
//	principal_id      one actor's decisions
//	decision_outcome  GRANTED | DENIED — refused if it is neither
//	action_type       one action
//	legal_entity_id   one entity's decisions
//	decided_from      RFC3339, inclusive
//	decided_to        RFC3339, exclusive
//	limit             1..200, default 50
//	cursor            continuation token from a previous page's next_cursor
//
// Response: 200 a page / 400 an unparseable date, limit, cursor or outcome /
// 401 missing principal or tenant scope / 503 unavailable.
func (h *Handler) ListAccessDecisions(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()

	params := domain.ListAccessDecisionsParams{
		PrincipalID:   strings.TrimSpace(q.Get("principal_id")),
		Outcome:       strings.ToUpper(strings.TrimSpace(q.Get("decision_outcome"))),
		ActionType:    strings.TrimSpace(q.Get("action_type")),
		LegalEntityID: strings.TrimSpace(q.Get("legal_entity_id")),
	}

	// An outcome that is neither GRANTED nor DENIED is REFUSED, not passed
	// through to match nothing. A listing that is empty because of a typo
	// ("Denied", "DENY") looks exactly like a tenant that has denied nobody,
	// and on an audit read those two answers must not be confusable.
	if params.Outcome != "" && !decisionOutcomes[params.Outcome] {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_filter",
			"field":   "decision_outcome",
			"message": domain.ErrInvalidDecisionOutcomeFilter.Error(),
		})
		return
	}

	// legal_entity_id is compared as a uuid in the store, so a malformed one
	// would surface as a driver error and be reported as a 503 — this
	// service's own instance of the platform-wide habit of reporting a bad
	// input as an outage, which known-gaps.md records for the /v1/authorize
	// path. Validated here so it is a 400 that names the field.
	if params.LegalEntityID != "" && !validScope(params.LegalEntityID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_filter",
			"field":   "legal_entity_id",
			"message": "legal_entity_id must be a UUID",
		})
		return
	}

	if from, ok := parseTimeParam(w, q.Get("decided_from"), "decided_from"); !ok {
		return
	} else if from != nil {
		params.DecidedFrom = from
	}
	if to, ok := parseTimeParam(w, q.Get("decided_to"), "decided_to"); !ok {
		return
	} else if to != nil {
		params.DecidedTo = to
	}

	// An inverted window is refused rather than answered with an empty page,
	// for the same reason a bad outcome filter is: it returns nothing, and
	// nothing is a legitimate answer to a well-formed question.
	if params.DecidedFrom != nil && params.DecidedTo != nil && !params.DecidedTo.After(*params.DecidedFrom) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_filter",
			"field":   "decided_to",
			"message": "decided_to must be after decided_from",
		})
		return
	}

	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_filter",
				"field":   "limit",
				"message": "limit must be a positive integer",
			})
			return
		}
		// A limit ABOVE the cap is clamped by the store rather than refused:
		// asking for more than the maximum is not a malformed request, and a
		// caller that says 1000 wants as much as it can have. The cap is
		// documented on domain.MaxAccessDecisionPageSize and the next_cursor
		// tells them there is more.
		params.Limit = n
	}

	cursor, err := domain.DecodeAccessDecisionCursor(strings.TrimSpace(q.Get("cursor")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_cursor",
			"field":   "cursor",
			"message": "cursor is not one this service issued — start from the first page rather than constructing one",
		})
		return
	}
	params.Cursor = cursor

	page, err := h.store.ListAccessDecisions(r.Context(), tenantScope, params)
	if err != nil {
		if errors.Is(err, domain.ErrTenantScopeRequired) {
			// Defence in depth behind requireTenant, which has already run.
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing_tenant_scope"})
			return
		}
		h.log.Error("ListAccessDecisions: store unavailable",
			zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, page)
}

// parseTimeParam reads an optional RFC3339 timestamp, writing a 400 and
// reporting false if it is present and unparseable.
//
// RFC3339 only, deliberately — not a list of accepted layouts. A date-only
// "2026-09-09" is rejected with a message naming the format rather than
// silently read as midnight UTC, because a caller who meant local midnight and
// got UTC midnight sees a window shifted by hours and no error.
func parseTimeParam(w http.ResponseWriter, raw, field string) (*time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_filter",
			"field":   field,
			"message": field + " must be an RFC3339 timestamp, e.g. 2026-09-09T00:00:00Z",
		})
		return nil, false
	}
	return &t, true
}
