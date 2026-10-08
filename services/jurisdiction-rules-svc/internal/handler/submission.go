package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 5: electronic invoicing and filing profiles, and the
// submission lifecycle (s13, s14).
//
// The profile (identity, syntax, mode, timing, dependencies, lifecycle) comes from
// the verified released pack. This service registers a submission under a
// profile and records the authority or provider status reports against that
// profile's lifecycle: a report can never regress a final outcome, a duplicate
// callback is stored once, and an acceptance needs a receipt. It does NOT transmit
// anything: adapters, endpoints and credentials belong to the provider layer.

func registerSubmissionRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/submission-profiles:resolve", h.ResolveSubmissionProfile)
	r.Post("/v1/regulatory-submissions", h.RegisterSubmission)
	r.Get("/v1/regulatory-submissions/{submission_id}", h.GetSubmission)
	r.Post("/v1/regulatory-submissions/{submission_id}/events", h.RecordSubmissionEvent)
}

var (
	sha256Re     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	upperSnakeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,39}$`)
)

func profileDomain(kind string) (string, string, bool) {
	switch kind {
	case "EINVOICE":
		return domain.EInvoiceDomain, domain.FamilyEInvoiceProfile, true
	case "FILING":
		return domain.FilingDomain, domain.FamilyFilingProfile, true
	}
	return "", "", false
}

// resolvedProfile is the result of resolving a profile and checking its dependencies.
type resolvedProfile struct {
	Decision *resolver.Decision
	Profile  *domain.SubmissionProfile
	Outcome  string
	Check    *domain.DependencyCheck
}

// resolveProfile resolves the profile in force and checks the dependencies a submission used.
func (h *Handler) resolveProfile(r *http.Request, kind, jurisdiction, code string, at time.Time, used []domain.DependencyUse, packRef, packVersion string) (*resolvedProfile, error) {
	dom, family, _ := profileDomain(kind)
	d, err := h.resolver.Resolve(r.Context(), resolver.Request{Jurisdiction: jurisdiction, RuleDomain: dom, RuleCode: code, At: at, PackRef: packRef, PackVersion: packVersion})
	if err != nil {
		return nil, err
	}
	out := &resolvedProfile{Decision: d, Outcome: d.Outcome}
	if d.Outcome != domain.OutcomeResolved || d.Rule == nil {
		return out, nil
	}
	p, perr := domain.ParseRuleParameters(d.Rule.Parameters)
	if len(d.Rule.Parameters) == 0 || perr != nil || p.Family != family || p.Profile == nil {
		out.Outcome = domain.CalcNoParameters
		return out, nil
	}
	out.Profile = p.Profile
	chk := p.Profile.CheckDependencies(used, at)
	out.Check, out.Outcome = &chk, chk.Outcome
	return out, nil
}

type ProfileRequest struct {
	Jurisdiction string `json:"jurisdiction"`
	// Kind is EINVOICE or FILING.
	Kind        string                 `json:"kind"`
	ProfileCode string                 `json:"profile_code"`
	EffectiveAt string                 `json:"effective_at"`
	Used        []domain.DependencyUse `json:"dependencies_used"`
	PackRef     string                 `json:"pack_ref"`
	PackVersion string                 `json:"pack_version"`
}

type ProfileResponse struct {
	Outcome    string                    `json:"outcome"`
	Resolution *resolver.Decision        `json:"resolution"`
	Profile    *domain.SubmissionProfile `json:"profile,omitempty"`
	Check      *domain.DependencyCheck   `json:"dependency_check,omitempty"`
}

func (h *Handler) parseProfileRequest(w http.ResponseWriter, req *ProfileRequest) (time.Time, bool) {
	if field := firstBlank(requiredField{"jurisdiction", req.Jurisdiction}, requiredField{"kind", req.Kind},
		requiredField{"profile_code", req.ProfileCode}, requiredField{"effective_at", req.EffectiveAt}); field != "" {
		writeMissingField(w, field)
		return time.Time{}, false
	}
	if _, _, ok := profileDomain(req.Kind); !ok {
		writeError(w, http.StatusBadRequest, "invalid_kind", "kind must be EINVOICE or FILING")
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, req.EffectiveAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_effective_at", "effective_at must be an RFC3339 instant")
		return time.Time{}, false
	}
	for _, u := range req.Used {
		if strings.TrimSpace(u.Ref) == "" || strings.TrimSpace(u.Version) == "" {
			writeError(w, http.StatusBadRequest, "invalid_dependencies_used", "each dependencies_used entry needs ref and version")
			return time.Time{}, false
		}
	}
	return at.UTC(), true
}

// ResolveSubmissionProfile answers which e-invoice or filing profile applies and,
// when the caller states the schema, code-list and rule versions it built the
// payload with, whether each is declared by the profile and valid on the date.
//
//	200 PROFILE_RESOLVED or NO_RULE      409 AMBIGUOUS
//	422 UNSUPPORTED_JURISDICTION, DEPENDENCY_UNKNOWN, DEPENDENCY_EXPIRED, NO_PARAMETERS      503 pack_unverified
func (h *Handler) ResolveSubmissionProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "submission_profile", "resolve")
	if !ok {
		return
	}
	var req ProfileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	at, ok := h.parseProfileRequest(w, &req)
	if !ok {
		return
	}
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	canon := req
	canon.EffectiveAt = at.Format(time.RFC3339Nano)
	rawReq, _ := json.Marshal(canon)
	res, err := h.resolveProfile(r, req.Kind, req.Jurisdiction, req.ProfileCode, at, req.Used, req.PackRef, req.PackVersion)
	if err != nil {
		h.writeResolveError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	body := ProfileResponse{Outcome: res.Outcome, Resolution: res.Decision, Profile: res.Profile, Check: res.Check}
	h.finishRecordDecision(w, r, "PROFILE", actor, idemKey, rawReq, at, res.Decision, res.Outcome, body, "profile")
}

type RegisterSubmissionRequest struct {
	ProfileRequest
	// SubjectRef identifies the invoice or filing in the caller's system.
	SubjectRef  string            `json:"subject_ref"`
	PayloadHash string            `json:"payload_hash"`
	Approvals   []domain.Approval `json:"approvals"`
}

// RegisterSubmission registers a submission under the profile in force. It is
// refused, and nothing is registered, when a dependency is unknown or expired
// (JUR-NEG-08) or when a filing lacks the approvals its profile requires.
//
//	201 registered (200 on an idempotent repeat)
//	409 AMBIGUOUS, idempotency_conflict      422 any refusal, with its outcome      503 pack_unverified
func (h *Handler) RegisterSubmission(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_submission", "create")
	if !ok {
		return
	}
	var req RegisterSubmissionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	at, ok := h.parseProfileRequest(w, &req.ProfileRequest)
	if !ok {
		return
	}
	if field := firstBlank(requiredField{"subject_ref", req.SubjectRef}, requiredField{"payload_hash", req.PayloadHash}); field != "" {
		writeMissingField(w, field)
		return
	}
	if len(req.SubjectRef) > 256 || !sha256Re.MatchString(req.PayloadHash) {
		writeError(w, http.StatusBadRequest, "invalid_payload", "subject_ref is at most 256 characters and payload_hash must be sha256:<64 hex>")
		return
	}
	idemKey, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	corr := r.Header.Get("X-Correlation-ID")
	canon := req
	canon.EffectiveAt = at.Format(time.RFC3339Nano)
	rawReq, _ := json.Marshal(canon)
	reqDigest, _ := domain.DigestOf(rawReq)

	res, err := h.resolveProfile(r, req.Kind, req.Jurisdiction, req.ProfileCode, at, req.Used, req.PackRef, req.PackVersion)
	if err != nil {
		h.writeResolveError(w, err, corr)
		return
	}
	outcome, check := res.Outcome, res.Check
	if outcome == domain.OutcomeProfileResolved && req.Kind == "FILING" && len(res.Profile.ApprovalRoles) > 0 {
		c := res.Profile.CheckApprovals(actor, req.Approvals)
		if c.Outcome == domain.ApprovalMissing {
			outcome, check = domain.ApprovalMissing, &c
		}
	}
	if outcome != domain.OutcomeProfileResolved {
		// Refused: record the decision as evidence, register nothing.
		body := ProfileResponse{Outcome: outcome, Resolution: res.Decision, Profile: res.Profile, Check: check}
		h.finishRecordDecision(w, r, "PROFILE", actor, nil, rawReq, at, res.Decision, outcome, body, "profile")
		return
	}

	snap, _ := json.Marshal(res.Profile)
	used, _ := json.Marshal(nonNilUses(req.Used))
	appr, _ := json.Marshal(nonNilApprovals(req.Approvals))
	rec, replayed, err := h.registry.CreateSubmission(r.Context(), store.CreateSubmissionParams{
		Kind: req.Kind, JurisdictionCode: req.Jurisdiction, ProfileCode: req.ProfileCode, SubjectRef: req.SubjectRef, PayloadHash: req.PayloadHash,
		InitialStatus: res.Profile.Lifecycle.Initial, EffectiveAt: at, PackVersionID: res.Decision.Pack.PackVersionID, ArtifactDigest: res.Decision.Pack.ArtifactDigest,
		RuleID: res.Decision.Rule.RuleID, RuleContentDigest: res.Decision.Rule.ContentDigest, ProfileSnapshot: snap, DependenciesUsed: used, Approvals: appr,
		SubmittedBy: actor, RequestDigest: reqDigest, IdempotencyKey: idemKey})
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
			return
		}
		h.writeRegistryError(w, err, corr)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"replayed": replayed, "submission": rec})
}

func nonNilUses(u []domain.DependencyUse) []domain.DependencyUse {
	if u == nil {
		return []domain.DependencyUse{}
	}
	return u
}

func nonNilApprovals(a []domain.Approval) []domain.Approval {
	if a == nil {
		return []domain.Approval{}
	}
	return a
}

// GetSubmission returns a submission with every status report it received.
func (h *Handler) GetSubmission(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "regulatory_submission", "view"); !ok {
		return
	}
	sub, events, err := h.registry.GetSubmission(r.Context(), chi.URLParam(r, "submission_id"))
	if err != nil {
		h.writeSubmissionError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"submission": sub, "events": events})
}

func (h *Handler) writeSubmissionError(w http.ResponseWriter, err error, corr string) {
	if errors.Is(err, domain.ErrSubmissionNotFound) {
		writeError(w, http.StatusNotFound, "submission_not_found", err.Error())
		return
	}
	h.writeRegistryError(w, err, corr)
}

type SubmissionEventRequest struct {
	ProviderEventID string          `json:"provider_event_id"`
	Status          string          `json:"status"`
	ReceiptID       string          `json:"receipt_id"`
	OccurredAt      string          `json:"occurred_at"`
	Detail          json.RawMessage `json:"detail"`
}

// RecordSubmissionEvent records an authority or provider status report.
//
// Always 200 once the report is stored: the body says whether it changed the status
// (applied) and why not otherwise (disposition), so a webhook caller is never
// pushed into a retry storm by a late, duplicate or out-of-order report.
func (h *Handler) RecordSubmissionEvent(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "regulatory_submission", "record_event")
	if !ok {
		return
	}
	var req SubmissionEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"provider_event_id", req.ProviderEventID}, requiredField{"status", req.Status}, requiredField{"occurred_at", req.OccurredAt}); field != "" {
		writeMissingField(w, field)
		return
	}
	if len(req.ProviderEventID) > 128 || len(req.ReceiptID) > 256 || !upperSnakeRe.MatchString(req.Status) {
		writeError(w, http.StatusBadRequest, "invalid_event", "provider_event_id is at most 128 characters, receipt_id at most 256, and status must be UPPER_SNAKE")
		return
	}
	occurred, err := time.Parse(time.RFC3339Nano, req.OccurredAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_occurred_at", "occurred_at must be an RFC3339 instant")
		return
	}
	if len(req.Detail) > 0 && string(req.Detail) != "null" {
		var obj map[string]any
		if json.Unmarshal(req.Detail, &obj) != nil || len(req.Detail) > 16*1024 {
			writeError(w, http.StatusBadRequest, "invalid_detail", "detail must be a JSON object of at most 16 KiB")
			return
		}
	} else {
		req.Detail = nil
	}
	res, err := h.registry.RecordSubmissionEvent(r.Context(), chi.URLParam(r, "submission_id"), store.SubmissionEventParams{
		ProviderEventID: req.ProviderEventID, Status: req.Status, ReceiptID: req.ReceiptID, OccurredAt: occurred.UTC(), Detail: req.Detail, RecordedBy: actor})
	if err != nil {
		h.writeSubmissionError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"replayed": res.Replayed, "applied": res.Event.Disposition == domain.DispApplied, "disposition": res.Event.Disposition,
		"status": res.Submission.Status, "is_terminal": res.Submission.IsTerminal, "event": res.Event})
}
