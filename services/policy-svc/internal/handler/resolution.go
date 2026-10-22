package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/resolve"
)

// resolutionQuery is the query string shared by the resolution reads:
//
//	policy_type=X       required
//	tenant_id=Y         optional — the caller's own tenant, or omit for global only
//	legal_entity_id=Z   optional (or the X-Legal-Entity-Id header)
//	as_of=RFC3339       optional business time (default now)
//	known_at=RFC3339    optional knowledge cut (default: as_of if past, else now)
type resolutionQuery struct {
	policyType    string
	tenantID      *string
	legalEntityID *string
	asOf, knownAt time.Time
}

func (h *Handler) parseResolutionQuery(w http.ResponseWriter, r *http.Request) (resolutionQuery, bool) {
	q := r.URL.Query()
	var out resolutionQuery
	out.policyType = q.Get("policy_type")
	if out.policyType == "" {
		missingField(w, "policy_type")
		return out, false
	}
	if tooLong(w, "policy_type", out.policyType, 64) {
		return out, false
	}
	parseTime := func(name string) (*time.Time, bool) {
		v := q.Get(name)
		if v == "" {
			return nil, true
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid_"+name, name+" must be a valid RFC3339 timestamp", nil)
			return nil, false
		}
		return &t, true
	}
	asOf, ok := parseTime("as_of")
	if !ok {
		return out, false
	}
	knownAt, ok := parseTime("known_at")
	if !ok {
		return out, false
	}

	verifiedTenant, ok := h.requireTenant(w, r)
	if !ok {
		return out, false
	}
	if v := q.Get("tenant_id"); v != "" {
		out.tenantID = &v
	}
	if !optionalUUID(w, "tenant_id", out.tenantID) || h.refuseForeignTenant(w, out.tenantID, verifiedTenant) {
		return out, false
	}
	var le *string
	if v := q.Get("legal_entity_id"); v != "" {
		le = &v
	}
	if out.legalEntityID, ok = legalEntityFromRequest(w, r, le); !ok {
		return out, false
	}

	a, k, ok := resolve.Times(asOf, knownAt, h.opts.Now().UTC())
	if !ok {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "invalid_known_at", "known_at cannot be in the future", nil)
		return out, false
	}
	out.asOf, out.knownAt = a, k
	return out, true
}

func (h *Handler) resolveQuery(w http.ResponseWriter, r *http.Request, op string) (resolve.Result, bool) {
	q, ok := h.parseResolutionQuery(w, r)
	if !ok {
		return resolve.Result{}, false
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, q.legalEntityID); !ok {
		return resolve.Result{}, false
	}
	candidates, err := h.candidates.Candidates(r.Context(), q.policyType, q.tenantID, q.legalEntityID)
	if err != nil {
		h.storeUnavailable(w, r, op, err)
		return resolve.Result{}, false
	}
	return resolve.Resolve(candidates, q.asOf, q.knownAt), true
}

// ListApplicablePolicyVersions handles GET /v1/policies — "get applicable
// policy set" (Doc 03 §8.1): the version governing each policy of the type
// for the scope at as_of / known_at, most specific scope first. A version
// that is not yet effective, has expired, or was suspended is not in the set.
func (h *Handler) ListApplicablePolicyVersions(w http.ResponseWriter, r *http.Request) {
	res, ok := h.resolveQuery(w, r, "ListApplicablePolicyVersions")
	if !ok {
		return
	}
	out := res.Applicable
	if out == nil {
		out = []*domain.ApplicablePolicyVersion{}
	}
	writeJSON(w, http.StatusOK, out)
}

// ResolveEffectivePolicy handles GET /v1/policies/resolve — GOV-05
// ResolveEffectivePolicy, and Doc 03's "validate threshold applicability":
// status RESOLVED (with the governing version), NOT_APPLICABLE or CONFLICTED
// (with the tied versions). Always 200; the status is the answer.
func (h *Handler) ResolveEffectivePolicy(w http.ResponseWriter, r *http.Request) {
	res, ok := h.resolveQuery(w, r, "ResolveEffectivePolicy")
	if !ok {
		return
	}
	res.Trace = nil
	writeJSON(w, http.StatusOK, res)
}

// ExplainPolicyResolution handles GET /v1/policies/explain — GOV-05
// ExplainPolicyResolution: the resolution plus, for every candidate version,
// whether it governs and why not when it does not. The explanation is the
// executed trace itself, never authored separately (V-001 PDC-I-18).
func (h *Handler) ExplainPolicyResolution(w http.ResponseWriter, r *http.Request) {
	res, ok := h.resolveQuery(w, r, "ExplainPolicyResolution")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ── GET /v1/policy-versions/compare ──────────────────────────────────────────

type fieldChange struct {
	Field  string `json:"field"`
	Base   any    `json:"base"`
	Target any    `json:"target"`
}

type payloadChange struct {
	Path   string `json:"path"`
	Change string `json:"change"` // added | removed | changed
	Base   any    `json:"base,omitempty"`
	Target any    `json:"target,omitempty"`
}

type comparison struct {
	Base           *domain.PolicyVersion `json:"base"`
	Target         *domain.PolicyVersion `json:"target"`
	SameContent    bool                  `json:"same_content"`
	FieldChanges   []fieldChange         `json:"field_changes"`
	PayloadChanges []payloadChange       `json:"payload_changes"`
}

func decodeNumbers(raw []byte) any {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&v)
	return v
}

// diffJSON records the differences between two decoded JSON values under
// path. Objects are compared key by key; anything else as a whole value.
func diffJSON(path string, a, b any, out *[]payloadChange) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			p := k
			if path != "" {
				p = path + "." + k
			}
			av, ina := am[k]
			bv, inb := bm[k]
			switch {
			case ina && !inb:
				*out = append(*out, payloadChange{Path: p, Change: "removed", Base: av})
			case !ina && inb:
				*out = append(*out, payloadChange{Path: p, Change: "added", Target: bv})
			default:
				diffJSON(p, av, bv, out)
			}
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, payloadChange{Path: path, Change: "changed", Base: a, Target: b})
	}
}

// ComparePolicyVersions handles GET /v1/policy-versions/compare?base=&target=
// — GOV-05 ComparePolicyVersions: the metadata fields and rule payload paths
// that differ between two versions visible to the caller.
func (h *Handler) ComparePolicyVersions(w http.ResponseWriter, r *http.Request) {
	baseID, targetID := r.URL.Query().Get("base"), r.URL.Query().Get("target")
	if baseID == "" {
		missingField(w, "base")
		return
	}
	if targetID == "" {
		missingField(w, "target")
		return
	}
	if !requireUUID(w, "base", baseID) || !requireUUID(w, "target", targetID) {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	var versions [2]*domain.PolicyVersion
	for i, id := range []string{baseID, targetID} {
		v, err := h.store.FindPolicyVersionByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, domain.ErrPolicyVersionNotFound) {
				writeError(w, http.StatusNotFound, codeNotFound, "policy_version_not_found", "", map[string]any{"policy_version_id": id})
				return
			}
			h.storeUnavailable(w, r, "ComparePolicyVersions", err)
			return
		}
		versions[i] = v
	}
	base, target := versions[0], versions[1]

	out := comparison{Base: base, Target: target, FieldChanges: []fieldChange{}, PayloadChanges: []payloadChange{}}
	timePtr := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return t.UTC()
	}
	fields := []fieldChange{
		{"policy_id", base.PolicyID, target.PolicyID},
		{"scope_type", base.ScopeType, target.ScopeType},
		{"tenant_id", base.TenantID, target.TenantID},
		{"legal_entity_id", base.LegalEntityID, target.LegalEntityID},
		{"version_number", base.VersionNumber, target.VersionNumber},
		{"version_status", base.VersionStatus, target.VersionStatus},
		{"effective_from", base.EffectiveFrom.UTC(), target.EffectiveFrom.UTC()},
		{"effective_to", timePtr(base.EffectiveTo), timePtr(target.EffectiveTo)},
		{"artifact_digest", base.ArtifactDigest, target.ArtifactDigest},
		{"source", base.Source, target.Source},
		{"rationale", base.Rationale, target.Rationale},
	}
	for _, f := range fields {
		bj, _ := json.Marshal(f.Base)
		tj, _ := json.Marshal(f.Target)
		if !bytes.Equal(bj, tj) {
			out.FieldChanges = append(out.FieldChanges, f)
		}
	}
	diffJSON("", decodeNumbers(base.RulePayload), decodeNumbers(target.RulePayload), &out.PayloadChanges)
	out.SameContent = len(out.PayloadChanges) == 0
	writeJSON(w, http.StatusOK, out)
}

// GetEvaluation handles GET /v1/policy-evaluations/{evidence_id}: the stored
// evidence of one of the caller's tenant's evaluations, with its ledger
// delivery state (V-001 §19 "retrieve authorized result/evidence summary";
// never cross-tenant).
func (h *Handler) GetEvaluation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "evidence_id")
	if !requireUUID(w, "evidence_id", id) {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	ev, err := h.store.FindEvaluationByID(r.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, domain.ErrEvaluationNotFound) {
			writeError(w, http.StatusNotFound, codeNotFound, "evaluation_not_found", "", map[string]any{"evidence_id": id})
			return
		}
		h.storeUnavailable(w, r, "GetEvaluation", err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}
