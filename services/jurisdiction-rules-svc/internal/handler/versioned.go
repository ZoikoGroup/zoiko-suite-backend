package handler

// Optimistic concurrency for every aggregate with commands (API-3, R6; REF-01
// and REF-10 "all protected changes require authoritative current version").
//
// An aggregate's ETag is its row_version (000020 for jurisdictions and rules,
// 000034 for the rest). The single read of an aggregate answers its ETag; a
// command on it takes an optional If-Match:
//
//   - If-Match names the current version → the row is locked FOR UPDATE in
//     the request's transaction (unit of work) and the command runs; nobody
//     can change the row between the check and the write;
//   - If-Match names an older version → 412 precondition_failed, nothing runs;
//   - no If-Match (or "*") → the command runs as before (existing callers keep
//     working; the console always sends it).
//
// A successful command answers the aggregate's new ETag, and the registry
// event it publishes carries that version as aggregate_version (J-12).
//
// jurisdictions and jurisdiction_rules keep their own handling in handler.go
// (their store methods take the expected version); the routes below are the
// rest, attached by route pattern through versionedRouter so no registration
// line changes.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// aggregateRef locates the row a route acts on: table and where are constants;
// params are the route parameters bound to where's $1..$n in order.
type aggregateRef struct {
	table  string
	where  string
	params []string
}

var (
	refSource = aggregateRef{"regulatory_sources", "source_id::text = $1", []string{"source_id"}}
	refInterp = aggregateRef{"interpretation_records", "interpretation_id::text = $1", []string{"interpretation_id"}}
	refPackV  = aggregateRef{"jurisdiction_pack_versions",
		"pack_version_id = (SELECT v.pack_version_id FROM jurisdiction_pack_versions v JOIN jurisdiction_packs p USING (pack_id) WHERE p.pack_ref = $1 AND v.version = $2)",
		[]string{"pack_ref", "version"}}
	refKey  = aggregateRef{"pack_signing_keys", "key_ref = $1", []string{"key_ref"}}
	refCalV = aggregateRef{"regulatory_calendar_versions",
		"calendar_version_id = (SELECT v.calendar_version_id FROM regulatory_calendar_versions v JOIN regulatory_calendars c USING (calendar_id) WHERE c.calendar_code = $1 AND v.version::text = $2)",
		[]string{"calendar_code", "version"}}
	refObl   = aggregateRef{"obligation_rules", "obligation_rule_id::text = $1", []string{"obligation_rule_id"}}
	refTermV = aggregateRef{"payment_term_versions",
		"term_version_id = (SELECT v.term_version_id FROM payment_term_versions v JOIN payment_terms t USING (term_id) WHERE t.term_code = $1 AND v.version::text = $2)",
		[]string{"term_code", "version"}}
	refRollout = aggregateRef{"jurisdiction_rollouts", "rollout_id::text = $1", []string{"rollout_id"}}
	refCase    = aggregateRef{"remediation_cases", "case_id::text = $1", []string{"case_id"}}
	refNotice  = aggregateRef{"source_change_notices", "notice_id::text = $1", []string{"notice_id"}}
	refImport  = aggregateRef{"jurisdiction_import_batches", "batch_id::text = $1", []string{"batch_id"}}
	refAlias   = aggregateRef{"jurisdiction_aliases", "alias_id::text = $1 AND jurisdiction_id::text = $2", []string{"alias_id", "jurisdiction_id"}}
	refSubm    = aggregateRef{"regulatory_submissions", "submission_id::text = $1", []string{"submission_id"}}
)

// versionedRoutes maps each route pattern to the aggregate it reads or changes.
var versionedRoutes = map[string]aggregateRef{
	"/v1/sources/{source_id}":                                            refSource,
	"/v1/admin/sources/{source_id}/review":                               refSource,
	"/v1/admin/sources/{source_id}/supersede":                            refSource,
	"/v1/interpretations/{interpretation_id}":                            refInterp,
	"/v1/admin/interpretations/{interpretation_id}/approve":              refInterp,
	"/v1/packs/{pack_ref}/versions/{version}":                            refPackV,
	"/v1/packs/{pack_ref}/versions/{version}/verify":                     refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/submit-review":        refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/compile":              refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/sign":                 refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/test-bundle":          refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/test-runs":            refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/reviews":              refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/certify":              refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/publish":              refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/deploy":               refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/rollback":             refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/supersede":            refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/withdraw":             refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/block":                refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/unblock":              refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/hotfix":               refPackV,
	"/v1/admin/packs/{pack_ref}/versions/{version}/hotfix/retrospective": refPackV,
	"/v1/admin/pack-signing-keys/{key_ref}/retire":                       refKey,
	"/v1/admin/pack-signing-keys/{key_ref}/revoke":                       refKey,
	"/v1/calendars/{calendar_code}/versions/{version}":                   refCalV,
	"/v1/admin/calendars/{calendar_code}/versions/{version}/sources":     refCalV,
	"/v1/admin/calendars/{calendar_code}/versions/{version}/publish":     refCalV,
	"/v1/obligation-rules/{obligation_rule_id}":                          refObl,
	"/v1/admin/obligation-rules/{obligation_rule_id}/provenance":         refObl,
	"/v1/admin/obligation-rules/{obligation_rule_id}/publish":            refObl,
	"/v1/payment-terms/{term_code}/versions/{version}":                   refTermV,
	"/v1/admin/payment-terms/{term_code}/versions/{version}/approve":     refTermV,
	"/v1/admin/payment-terms/{term_code}/versions/{version}/supersede":   refTermV,
	"/v1/admin/payment-terms/{term_code}/versions/{version}/retire":      refTermV,
	"/v1/admin/rollouts/{rollout_id}":                                    refRollout,
	"/v1/admin/rollouts/{rollout_id}/attestations":                       refRollout,
	"/v1/admin/rollouts/{rollout_id}/expert-approvals":                   refRollout,
	"/v1/admin/rollouts/{rollout_id}/transition":                         refRollout,
	"/v1/admin/rollouts/{rollout_id}/owner":                              refRollout,
	"/v1/admin/rollouts/{rollout_id}/packs":                              refRollout,
	"/v1/admin/remediation-cases/{case_id}":                              refCase,
	"/v1/admin/remediation-cases/{case_id}/close":                        refCase,
	"/v1/admin/remediation-cases/{case_id}/dispositions":                 refCase,
	"/v1/source-change-notices/{notice_id}":                              refNotice,
	"/v1/admin/source-change-notices/{notice_id}/review":                 refNotice,
	"/v1/admin/source-change-notices/{notice_id}/close":                  refNotice,
	"/v1/admin/jurisdiction-imports/{batch_id}":                          refImport,
	"/v1/admin/jurisdiction-imports/{batch_id}/approve":                  refImport,
	"/v1/admin/jurisdiction-imports/{batch_id}/reject":                   refImport,
	"/v1/admin/jurisdictions/{jurisdiction_id}/aliases/{alias_id}/end":   refAlias,
	"/v1/regulatory-submissions/{submission_id}":                         refSubm,
	"/v1/regulatory-submissions/{submission_id}/events":                  refSubm,
}

// createRoutes create an aggregate: the event they publish is its version 1.
var createRoutes = map[string]bool{
	"/v1/admin/regimes": true, "/v1/admin/sources": true, "/v1/admin/interpretations": true,
	"/v1/admin/packs": true, "/v1/admin/packs/{pack_ref}/versions": true, "/v1/admin/pack-signing-keys": true,
	"/v1/admin/calendars": true, "/v1/admin/calendars/{calendar_code}/versions": true,
	"/v1/admin/obligation-rules": true, "/v1/admin/payment-terms": true, "/v1/admin/payment-terms/{term_code}/versions": true,
	"/v1/admin/rollouts": true, "/v1/admin/remediation-cases": true, "/v1/admin/source-change-notices": true,
	"/v1/admin/deployment-rings": true, "/v1/admin/deployment-regions": true, "/v1/admin/jurisdiction-imports": true,
	"/v1/admin/jurisdictions/{jurisdiction_id}/aliases": true, "/v1/regulatory-submissions": true,
	"/v1/admin/jurisdictions/{jurisdiction_id}/rules": true,
}

// VersionStore reads aggregate versions (store.PgStore).
type VersionStore interface {
	AggregateVersion(ctx context.Context, table, where string, lock bool, args ...any) (int64, bool, error)
}

// WithAggregateVersions turns on ETag and If-Match for the routes above.
func (h *Handler) WithAggregateVersions(v VersionStore) *Handler {
	h.versions = v
	return h
}

type boundAggregateKey struct{}

// boundAggregate is the aggregate a request acts on, for the event version.
type boundAggregate struct {
	ref     aggregateRef
	args    []any
	created bool
}

// eventAggregateVersion is the version a registry event announces (J-12): the
// aggregate's row_version after the command, or 1 for a create. 0 = unknown.
func (h *Handler) eventAggregateVersion(ctx context.Context) int64 {
	b, _ := ctx.Value(boundAggregateKey{}).(*boundAggregate)
	switch {
	case b == nil:
		return 0
	case b.created:
		return 1
	case h.versions == nil:
		return 0
	}
	v, found, err := h.versions.AggregateVersion(ctx, b.ref.table, b.ref.where, false, b.args...)
	if err != nil || !found {
		return 0
	}
	return v
}

// versionedRouter attaches the concurrency handling by route pattern.
type versionedRouter struct {
	chi.Router
	h *Handler
}

func (vr versionedRouter) Get(pattern string, fn http.HandlerFunc) {
	vr.Router.Get(pattern, vr.h.versioned(pattern, fn))
}
func (vr versionedRouter) Post(pattern string, fn http.HandlerFunc) {
	vr.Router.Post(pattern, vr.h.versioned(pattern, fn))
}
func (vr versionedRouter) Put(pattern string, fn http.HandlerFunc) {
	vr.Router.Put(pattern, vr.h.versioned(pattern, fn))
}

func (h *Handler) versioned(pattern string, fn http.HandlerFunc) http.HandlerFunc {
	if createRoutes[pattern] {
		return func(w http.ResponseWriter, r *http.Request) {
			fn(w, r.WithContext(context.WithValue(r.Context(), boundAggregateKey{}, &boundAggregate{created: true})))
		}
	}
	ref, ok := versionedRoutes[pattern]
	if !ok {
		return fn
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if h.versions == nil {
			fn(w, r)
			return
		}
		args := make([]any, len(ref.params))
		for i, p := range ref.params {
			args[i] = chi.URLParam(r, p)
		}
		ctx := context.WithValue(r.Context(), boundAggregateKey{}, &boundAggregate{ref: ref, args: args})
		r = r.WithContext(ctx)

		if r.Method == http.MethodGet {
			if v, found, err := h.versions.AggregateVersion(ctx, ref.table, ref.where, false, args...); err == nil && found {
				setETag(w, v)
			}
			fn(w, r)
			return
		}

		ifMatch, ok := parseIfMatch(w, r)
		if !ok {
			return
		}
		if ifMatch != nil {
			current, found, err := h.versions.AggregateVersion(ctx, ref.table, ref.where, true, args...)
			if err != nil {
				writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "")
				return
			}
			if found && current != *ifMatch {
				w.Header().Set("ETag", etag(current))
				writeError(w, r, http.StatusPreconditionFailed, "precondition_failed",
					"If-Match "+strconv.FormatInt(*ifMatch, 10)+" is not the current version ("+strconv.FormatInt(current, 10)+"): re-read and retry")
				return
			}
		}
		fn(w, r)
		// The response is held until the unit commits, so its headers can
		// still carry the version the command produced.
		if v, found, err := h.versions.AggregateVersion(ctx, ref.table, ref.where, false, args...); err == nil && found {
			w.Header().Set("ETag", etag(v))
		}
	}
}
