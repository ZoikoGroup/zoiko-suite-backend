package handler

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 7: release lifecycle, deployment rings and regions,
// rollback, emergency hotfix, source-change intake and the operations
// metrics feed. Each privileged command is its own authorization action
// (s28): publish, withdraw, block, unblock, deploy, rollback and hotfix
// declaration can be granted to different roles.

// OpsPolicy holds operational parameters the specification leaves to
// controlled decisions (s24, s29, s38).
type OpsPolicy struct {
	// HotfixRetroSLA is how long after declaration the mandatory hotfix
	// retrospective may stay open before it is reported overdue.
	HotfixRetroSLA time.Duration
	// CertAgeWarnDays marks a certification stale in the metrics.
	CertAgeWarnDays int
	// UpcomingDays is the look-ahead for future-dated pack windows.
	UpcomingDays int
	// MetricsWindowHours is the default window for resolution and failure counts.
	MetricsWindowHours int
}

// DefaultOpsPolicy is used when the deployment does not say otherwise.
var DefaultOpsPolicy = OpsPolicy{HotfixRetroSLA: 120 * time.Hour, CertAgeWarnDays: 180, UpcomingDays: 30, MetricsWindowHours: 24}

// WithOperations sets the operational policy (zero fields keep the defaults).
func (h *Handler) WithOperations(p OpsPolicy) *Handler {
	d := DefaultOpsPolicy
	if p.HotfixRetroSLA > 0 {
		d.HotfixRetroSLA = p.HotfixRetroSLA
	}
	if p.CertAgeWarnDays > 0 {
		d.CertAgeWarnDays = p.CertAgeWarnDays
	}
	if p.UpcomingDays > 0 {
		d.UpcomingDays = p.UpcomingDays
	}
	if p.MetricsWindowHours > 0 {
		d.MetricsWindowHours = p.MetricsWindowHours
	}
	h.ops = d
	return h
}

func registerOpsRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/deployment-regions", h.ListRegions)
	r.Get("/v1/deployment-rings", h.ListRings)
	r.Get("/v1/packs/{pack_ref}/versions/{version}/deployments", h.ListDeployments)
	r.Get("/v1/packs/{pack_ref}/versions/{version}/hotfix", h.GetHotfix)
	r.Get("/v1/source-change-notices", h.ListNotices)
	r.Get("/v1/source-change-notices/{notice_id}", h.GetNotice)
	r.Get("/v1/ops/metrics", h.OpsMetrics)

	r.Post("/v1/admin/deployment-regions", h.CreateRegion)
	r.Post("/v1/admin/deployment-rings", h.CreateRing)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/publish", h.PublishPackVersion)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/withdraw", h.WithdrawPackVersion)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/block", h.BlockPackVersion)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/unblock", h.UnblockPackVersion)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/deploy", h.DeployPackVersion)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/rollback", h.RollbackDeployment)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/hotfix", h.DeclareHotfix)
	r.Post("/v1/admin/packs/{pack_ref}/versions/{version}/hotfix/retrospective", h.CompleteRetrospective)
	r.Post("/v1/admin/source-change-notices", h.CreateNotice)
	r.Post("/v1/admin/source-change-notices/{notice_id}/review", h.StartNoticeReview)
	r.Post("/v1/admin/source-change-notices/{notice_id}/close", h.CloseNotice)
}

func (h *Handler) writeOpsError(w http.ResponseWriter, err error, corr string) {
	var blocked *store.OpsBlocked
	switch {
	case errors.As(err, &blocked):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "operation_blocked", "message": "one or more gates are not met", "reasons": blocked.Reasons})
	case errors.Is(err, domain.ErrInvalidLifecycle):
		writeError(w, http.StatusConflict, "invalid_lifecycle_state", err.Error())
	case errors.Is(err, domain.ErrRingNotFound):
		writeError(w, http.StatusNotFound, "ring_not_found", err.Error())
	case errors.Is(err, domain.ErrRegionNotFound):
		writeError(w, http.StatusNotFound, "region_not_found", err.Error())
	case errors.Is(err, domain.ErrDeploymentNotFound):
		writeError(w, http.StatusNotFound, "deployment_not_found", err.Error())
	case errors.Is(err, domain.ErrHotfixNotFound):
		writeError(w, http.StatusNotFound, "hotfix_not_found", err.Error())
	case errors.Is(err, domain.ErrNoticeNotFound):
		writeError(w, http.StatusNotFound, "notice_not_found", err.Error())
	case errors.Is(err, domain.ErrNotAssignedReviewer):
		writeError(w, http.StatusForbidden, "not_assigned_reviewer", err.Error())
	default:
		h.writeCertError(w, err, corr)
	}
}

func pathRefVersion(r *http.Request) (string, string) {
	return chi.URLParam(r, "pack_ref"), chi.URLParam(r, "version")
}

// ── regions and rings ───────────────────────────────────────────────────────

var scopeCodeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type CreateRegionRequest struct {
	RegionCode  string  `json:"region_code"`
	Description *string `json:"description"`
}

func (h *Handler) CreateRegion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "deployment_region", "create")
	if !ok {
		return
	}
	var req CreateRegionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !scopeCodeRe.MatchString(req.RegionCode) {
		writeError(w, http.StatusBadRequest, "invalid_region_code", "region_code must be lowercase letters, digits and . _ -")
		return
	}
	x, created, err := h.registry.CreateRegion(r.Context(), req.RegionCode, req.Description, actor)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		writeJSON(w, http.StatusCreated, x)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (h *Handler) ListRegions(w http.ResponseWriter, r *http.Request) {
	xs, err := h.registry.ListRegions(r.Context())
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"regions": xs})
}

type CreateRingRequest struct {
	RingCode       string  `json:"ring_code"`
	Ordinal        int     `json:"ordinal"`
	MinSoakSeconds int     `json:"min_soak_seconds"`
	Description    *string `json:"description"`
}

func (h *Handler) CreateRing(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "deployment_ring", "create")
	if !ok {
		return
	}
	var req CreateRingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !scopeCodeRe.MatchString(req.RingCode) {
		writeError(w, http.StatusBadRequest, "invalid_ring_code", "ring_code must be lowercase letters, digits and . _ -")
		return
	}
	if req.Ordinal < 1 || req.MinSoakSeconds < 0 {
		writeError(w, http.StatusBadRequest, "invalid_ring", "ordinal must be at least 1 and min_soak_seconds cannot be negative")
		return
	}
	x, created, err := h.registry.CreateRing(r.Context(), req.RingCode, req.Ordinal, req.MinSoakSeconds, req.Description, actor)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		writeJSON(w, http.StatusCreated, x)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (h *Handler) ListRings(w http.ResponseWriter, r *http.Request) {
	xs, err := h.registry.ListRings(r.Context())
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rings": xs})
}

// ── release lifecycle ───────────────────────────────────────────────────────

type ReasonRequest struct {
	Reason      string `json:"reason"`
	EvidenceRef string `json:"evidence_ref"`
}

func (h *Handler) lifecycle(w http.ResponseWriter, r *http.Request, action string, needReason, needEvidence bool,
	run func(ref, version, reason, evidence, actor string) (*store.ReleaseResult, error), event string) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", action)
	if !ok {
		return
	}
	var req ReasonRequest
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &req) {
			return
		}
	}
	if needReason && strings.TrimSpace(req.Reason) == "" {
		writeMissingField(w, "reason")
		return
	}
	if needEvidence && strings.TrimSpace(req.EvidenceRef) == "" {
		writeMissingField(w, "evidence_ref")
		return
	}
	ref, version := pathRefVersion(r)
	res, err := run(ref, version, req.Reason, req.EvidenceRef, actor)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if res.Changed {
		h.emitRegistry(r, event, ref+"@"+version, actor, map[string]any{"pack_ref": ref, "version": version,
			"from_status": res.FromStatus, "to_status": res.ToStatus, "reason": req.Reason, "evidence_ref": req.EvidenceRef})
	}
	writeJSON(w, http.StatusOK, res)
}

// PublishPackVersion releases a CERTIFIED version (re-verified, dependencies released).
func (h *Handler) PublishPackVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, "publish", false, false, func(ref, v, _, ev, actor string) (*store.ReleaseResult, error) {
		return h.registry.PublishPackVersion(r.Context(), ref, v, actor, ev)
	}, events.EventPackReleased)
}

// WithdrawPackVersion stops all new use. Reason and evidence are mandatory (s25).
func (h *Handler) WithdrawPackVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, "withdraw", true, true, func(ref, v, reason, ev, actor string) (*store.ReleaseResult, error) {
		return h.registry.WithdrawPackVersion(r.Context(), ref, v, reason, ev, actor)
	}, events.EventPackWithdrawn)
}

// BlockPackVersion is the emergency stop pending incident review.
func (h *Handler) BlockPackVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, "block", true, false, func(ref, v, reason, _, actor string) (*store.ReleaseResult, error) {
		return h.registry.BlockPackVersion(r.Context(), ref, v, reason, actor)
	}, events.EventPackBlocked)
}

// UnblockPackVersion returns a blocked version to RELEASED after review.
func (h *Handler) UnblockPackVersion(w http.ResponseWriter, r *http.Request) {
	h.lifecycle(w, r, "unblock", true, false, func(ref, v, reason, _, actor string) (*store.ReleaseResult, error) {
		return h.registry.UnblockPackVersion(r.Context(), ref, v, reason, actor)
	}, events.EventPackUnblocked)
}

// ── deployment ──────────────────────────────────────────────────────────────

type DeployRequest struct {
	Ring   string `json:"ring"`
	Region string `json:"region"`
	// ExceptionReason documents skipping ring order. Accepted only for a
	// declared hotfix and only from its incident commander (s24).
	ExceptionReason string `json:"exception_reason"`
}

// DeployPackVersion deploys (promotes) a RELEASED version to a ring and region.
func (h *Handler) DeployPackVersion(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "deploy")
	if !ok {
		return
	}
	var req DeployRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"ring", req.Ring}, requiredField{"region", req.Region}); field != "" {
		writeMissingField(w, field)
		return
	}
	ref, version := pathRefVersion(r)
	d, created, err := h.registry.DeployPackVersion(r.Context(), ref, version, req.Ring, req.Region, actor, req.ExceptionReason)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		h.emitRegistry(r, events.EventPackPromoted, d.DeploymentID, actor, map[string]any{"pack_ref": ref, "version": version,
			"ring": req.Ring, "region": req.Region, "exception": d.ExceptionReason != nil})
		writeJSON(w, http.StatusCreated, d)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type RollbackRequest struct {
	Ring           string `json:"ring"`
	Region         string `json:"region"`
	RestoreVersion string `json:"restore_version"`
	Reason         string `json:"reason"`
}

// RollbackDeployment rolls a version back in one ring and region and restores
// an older release there. It changes future resolution only.
func (h *Handler) RollbackDeployment(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "rollback")
	if !ok {
		return
	}
	var req RollbackRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"ring", req.Ring}, requiredField{"region", req.Region},
		requiredField{"restore_version", req.RestoreVersion}, requiredField{"reason", req.Reason}); field != "" {
		writeMissingField(w, field)
		return
	}
	ref, version := pathRefVersion(r)
	res, err := h.registry.RollbackDeployment(r.Context(), ref, version, req.Ring, req.Region, req.RestoreVersion, req.Reason, actor)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	h.emitRegistry(r, events.EventPackRolledBack, res.RolledBack.DeploymentID, actor, map[string]any{"pack_ref": ref, "version": version,
		"ring": req.Ring, "region": req.Region, "restored_version": req.RestoreVersion, "reason": req.Reason, "decisions_affected": res.DecisionsMade})
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) ListDeployments(w http.ResponseWriter, r *http.Request) {
	ref, version := pathRefVersion(r)
	ds, err := h.registry.ListDeployments(r.Context(), ref, version)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": ds})
}

// ── emergency hotfix ────────────────────────────────────────────────────────

type DeclareHotfixRequest struct {
	Severity              string `json:"severity"`
	ScopeSummary          string `json:"scope_summary"`
	IncidentRef           string `json:"incident_ref"`
	IncidentCommander     string `json:"incident_commander"`
	RollbackTargetVersion string `json:"rollback_target_version"`
}

// DeclareHotfix marks a DRAFT or REVIEW version as an emergency hotfix.
func (h *Handler) DeclareHotfix(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "declare_hotfix")
	if !ok {
		return
	}
	var req DeclareHotfixRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"scope_summary", req.ScopeSummary}, requiredField{"incident_ref", req.IncidentRef},
		requiredField{"incident_commander", req.IncidentCommander}, requiredField{"rollback_target_version", req.RollbackTargetVersion}); field != "" {
		writeMissingField(w, field)
		return
	}
	sev := strings.ToUpper(strings.TrimSpace(req.Severity))
	if sev != "P0" && sev != "P1" {
		writeError(w, http.StatusBadRequest, "invalid_severity", "severity must be P0 or P1; anything lower is not an emergency path")
		return
	}
	ref, version := pathRefVersion(r)
	hf, created, err := h.registry.DeclareHotfix(r.Context(), ref, version, sev, req.ScopeSummary, req.IncidentRef,
		req.IncidentCommander, req.RollbackTargetVersion, h.opsPolicy().HotfixRetroSLA, actor)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if created {
		h.emitRegistry(r, events.EventHotfixDeclared, ref+"@"+version, actor, map[string]any{"pack_ref": ref, "version": version,
			"severity": sev, "incident_ref": req.IncidentRef})
		writeJSON(w, http.StatusCreated, hf)
		return
	}
	writeJSON(w, http.StatusOK, hf)
}

func (h *Handler) opsPolicy() OpsPolicy {
	if h.ops.HotfixRetroSLA == 0 {
		return DefaultOpsPolicy
	}
	return h.ops
}

func (h *Handler) GetHotfix(w http.ResponseWriter, r *http.Request) {
	ref, version := pathRefVersion(r)
	hf, err := h.registry.GetHotfix(r.Context(), ref, version)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, hf)
}

type RetrospectiveRequest struct {
	Note string `json:"note"`
}

// CompleteRetrospective records the mandatory post-incident review, once.
func (h *Handler) CompleteRetrospective(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_pack_version", "complete_retrospective")
	if !ok {
		return
	}
	var req RetrospectiveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Note) == "" {
		writeMissingField(w, "note")
		return
	}
	ref, version := pathRefVersion(r)
	hf, changed, err := h.registry.CompleteRetrospective(r.Context(), ref, version, req.Note, actor)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventHotfixRetrospective, ref+"@"+version, actor, map[string]any{"pack_ref": ref, "version": version})
	}
	writeJSON(w, http.StatusOK, hf)
}

// ── source-change intake ────────────────────────────────────────────────────

// noticeTypes are the s31 change-management classes a notice may carry.
var noticeTypes = map[string]bool{"FUTURE_DATED_LAW": true, "SCHEMA_OR_CODE_LIST_UPDATE": true, "CLARIFICATION": true,
	"RETROACTIVE_CHANGE": true, "AUTHORITY_PROCESS_CHANGE": true, "SOURCE_WITHDRAWN": true, "DISPUTED_INTERPRETATION": true}

type CreateNoticeRequest struct {
	JurisdictionID   *string `json:"jurisdiction_id"`
	Authority        string  `json:"authority"`
	ChangeType       string  `json:"change_type"`
	Title            string  `json:"title"`
	Location         *string `json:"location"`
	ObservedHash     *string `json:"observed_hash"`
	AffectedSourceID *string `json:"affected_source_id"`
	Detail           *string `json:"detail"`
}

// CreateNotice records that an authority's material may have changed. It
// opens a controlled review; it never edits or publishes a rule (s29).
func (h *Handler) CreateNotice(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "source_change_notice", "create")
	if !ok {
		return
	}
	var req CreateNoticeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"authority", req.Authority}, requiredField{"change_type", req.ChangeType}, requiredField{"title", req.Title}); field != "" {
		writeMissingField(w, field)
		return
	}
	if !noticeTypes[strings.ToUpper(strings.TrimSpace(req.ChangeType))] {
		writeError(w, http.StatusBadRequest, "invalid_change_type",
			"change_type must be one of FUTURE_DATED_LAW, SCHEMA_OR_CODE_LIST_UPDATE, CLARIFICATION, RETROACTIVE_CHANGE, AUTHORITY_PROCESS_CHANGE, SOURCE_WITHDRAWN, DISPUTED_INTERPRETATION (ZS-JUR-001 s31)")
		return
	}
	if req.ObservedHash != nil && !domain.ValidDigest(*req.ObservedHash) {
		writeError(w, http.StatusBadRequest, "invalid_observed_hash", "observed_hash must be sha256:<64 hex>")
		return
	}
	n, err := h.registry.CreateNotice(r.Context(), store.CreateNoticeParams{JurisdictionID: req.JurisdictionID, AffectedSourceID: req.AffectedSourceID,
		Authority: req.Authority, ChangeType: strings.ToUpper(strings.TrimSpace(req.ChangeType)), Title: req.Title, Location: req.Location,
		ObservedHash: req.ObservedHash, Detail: req.Detail, CreatedBy: actor})
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	h.emitRegistry(r, events.EventSourceChangeCaptured, n.NoticeID, actor, map[string]any{"notice_id": n.NoticeID, "change_type": n.ChangeType, "authority": n.Authority})
	writeJSON(w, http.StatusCreated, n)
}

func (h *Handler) GetNotice(w http.ResponseWriter, r *http.Request) {
	n, err := h.registry.GetNotice(r.Context(), chi.URLParam(r, "notice_id"))
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (h *Handler) ListNotices(w http.ResponseWriter, r *http.Request) {
	limit, offset, ok := h.parsePaging(w, r.URL.Query())
	if !ok {
		return
	}
	ns, err := h.registry.ListNotices(r.Context(), strings.ToUpper(r.URL.Query().Get("status")), limit, offset)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notices": ns})
}

// StartNoticeReview assigns the caller as independent reviewer (not the intake author).
func (h *Handler) StartNoticeReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "source_change_notice", "review")
	if !ok {
		return
	}
	n, changed, err := h.registry.StartNoticeReview(r.Context(), chi.URLParam(r, "notice_id"), actor)
	if err != nil {
		if errors.Is(err, domain.ErrNotIndependent) {
			writeError(w, http.StatusForbidden, "segregation_of_duties", err.Error())
			return
		}
		h.writeRegistryError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventSourceChangeInReview, n.NoticeID, actor, map[string]any{"notice_id": n.NoticeID})
	}
	writeJSON(w, http.StatusOK, n)
}

type CloseNoticeRequest struct {
	Outcome                string  `json:"outcome"`
	Note                   string  `json:"note"`
	LinkedInterpretationID *string `json:"linked_interpretation_id"`
}

// CloseNotice records the review's conclusion. It changes no rule or pack.
func (h *Handler) CloseNotice(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "source_change_notice", "close")
	if !ok {
		return
	}
	var req CloseNoticeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	outcome := strings.ToUpper(strings.TrimSpace(req.Outcome))
	switch outcome {
	case "NO_CHANGE", "INTERPRETATION_RECORDED", "RULE_CHANGE_PLANNED", "CAPABILITY_BLOCKED":
	default:
		writeError(w, http.StatusBadRequest, "invalid_outcome", "outcome must be NO_CHANGE, INTERPRETATION_RECORDED, RULE_CHANGE_PLANNED or CAPABILITY_BLOCKED")
		return
	}
	if strings.TrimSpace(req.Note) == "" {
		writeMissingField(w, "note")
		return
	}
	n, changed, err := h.registry.CloseNotice(r.Context(), chi.URLParam(r, "notice_id"), actor, outcome, req.Note, req.LinkedInterpretationID)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	if changed {
		h.emitRegistry(r, events.EventSourceChangeClosed, n.NoticeID, actor, map[string]any{"notice_id": n.NoticeID, "outcome": outcome})
	}
	writeJSON(w, http.StatusOK, n)
}

// ── metrics ─────────────────────────────────────────────────────────────────

// OpsMetrics returns the s29 operations feed. It is data for a dashboard or
// an alert; no UI exists in this service.
func (h *Handler) OpsMetrics(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "pack_ops_metrics", "view"); !ok {
		return
	}
	p := h.opsPolicy()
	q := r.URL.Query()
	get := func(name string, def int) (int, bool) {
		v := q.Get(name)
		if v == "" {
			return def, true
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 24*365 {
			writeError(w, http.StatusBadRequest, "invalid_"+name, name+" must be a positive integer")
			return 0, false
		}
		return n, true
	}
	win, ok := get("window_hours", p.MetricsWindowHours)
	if !ok {
		return
	}
	age, ok := get("cert_age_warn_days", p.CertAgeWarnDays)
	if !ok {
		return
	}
	up, ok := get("upcoming_days", p.UpcomingDays)
	if !ok {
		return
	}
	m, err := h.registry.Metrics(r.Context(), win, age, up)
	if err != nil {
		h.writeOpsError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, m)
}
