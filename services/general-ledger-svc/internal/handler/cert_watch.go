package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/certwatch"
)

// The certification watch (ZS-SVC-Z-001 RB-05, DQC-017): what the ledger does when a certification it relied on is withdrawn. All
// three are authorized against the legal entity they concern, like every other GL action, and each has its own permission:
// sweeping is a routine act, acknowledging a flag is the recorded decision of a person.
const (
	actionCertWatchSweep      = "GL_CERTIFICATION_WATCH_SWEEP"
	actionCertFlagView        = "GL_CERTIFICATION_FLAG_VIEW"
	actionCertFlagAcknowledge = "GL_CERTIFICATION_FLAG_ACKNOWLEDGE"
)

// CertificationSweeper is what the handler needs of certwatch.Sweeper.
type CertificationSweeper interface {
	Sweep(ctx context.Context, tenant, principal, correlationID, legalEntityID string) (*certwatch.SweepResult, error)
}

// WithCertificationWatch wires the watch: the sweeper that checks watched certifications and the store of flags. Without it the
// three routes answer 503.
func (h *Handler) WithCertificationWatch(sweeper CertificationSweeper, store certwatch.Store) *Handler {
	h.cwSweeper, h.cwStore = sweeper, store
	return h
}

type sweepRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
}

type acknowledgeRequest struct {
	Acknowledgement      string `json:"acknowledgement"`
	RemediationJournalID string `json:"remediation_journal_id,omitempty"`
}

func strictJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func (h *Handler) certWatchWired(w http.ResponseWriter) bool {
	if h.cwSweeper == nil || h.cwStore == nil {
		writeError(w, http.StatusServiceUnavailable, "certification_watch_unavailable", "")
		return false
	}
	return true
}

func (h *Handler) certWatchError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, certwatch.ErrFlagNotFound):
		writeError(w, http.StatusNotFound, "certification_flag_not_found", "")
	case errors.Is(err, certwatch.ErrFlagAlreadyAcknowledged):
		writeError(w, http.StatusConflict, "certification_flag_already_acknowledged", err.Error())
	case errors.Is(err, certwatch.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_certification_watch_request", err.Error())
	default:
		h.log.Error("certification watch failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

// SweepCertifications checks every certification the legal entity relies on with the service that issued it, and flags what
// rests on any that was revoked. The caller's identity is forwarded to the issuing services, so the caller must be able to read
// those certifications.
func (h *Handler) SweepCertifications(w http.ResponseWriter, r *http.Request) {
	if !h.certWatchWired(w) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req sweepRequest
	if !strictJSON(w, r, &req) {
		return
	}
	if !isUUID(req.LegalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_legal_entity_id", "legal_entity_id must be a UUID")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCertWatchSweep); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	res, err := h.cwSweeper.Sweep(r.Context(), tenantID, principalID, r.Header.Get("X-Correlation-ID"), req.LegalEntityID)
	if err != nil {
		h.certWatchError(w, err)
		return
	}
	if h.cwObserver != nil {
		h.cwObserver.FlagsRaised(certwatch.TriggerManual, res.NewFlags)
	}
	writeJSON(w, http.StatusOK, res)
}

// ListCertificationFlags lists the flags of one legal entity, optionally only the OPEN or ACKNOWLEDGED ones.
func (h *Handler) ListCertificationFlags(w http.ResponseWriter, r *http.Request) {
	if !h.certWatchWired(w) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	for k, v := range q {
		if (k != "legal_entity_id" && k != "status") || len(v) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_query", "unsupported or repeated query parameter "+k)
			return
		}
	}
	entity := q.Get("legal_entity_id")
	if !isUUID(entity) {
		writeError(w, http.StatusBadRequest, "invalid_legal_entity_id", "legal_entity_id must be a UUID")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, actionCertFlagView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	flags, err := h.cwStore.ListFlags(r.Context(), tenantID, entity, q.Get("status"))
	if err != nil {
		h.certWatchError(w, err)
		return
	}
	open := 0
	for _, f := range flags {
		if f.Status == certwatch.FlagOpen {
			open++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": flags, "count": len(flags), "open": open})
}

// AcknowledgeCertificationFlag records a person's decision on a flag, authorized against the flagged journal's legal entity. It
// reverses nothing: a remediating journal, if there is one, is named, not created.
func (h *Handler) AcknowledgeCertificationFlag(w http.ResponseWriter, r *http.Request) {
	if !h.certWatchWired(w) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req acknowledgeRequest
	if !strictJSON(w, r, &req) {
		return
	}
	flagID := chi.URLParam(r, "flag_id")
	flag, err := h.cwStore.GetFlag(r.Context(), tenantID, flagID)
	if err != nil {
		h.certWatchError(w, err)
		return
	}
	// Authorization is checked against the flagged entity BEFORE the flag's state is reported on.
	if err := h.authz.CheckAllowed(r.Context(), principalID, flag.LegalEntityID, actionCertFlagAcknowledge); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	got, err := h.cwStore.Acknowledge(r.Context(), tenantID, flagID, principalID, req.Acknowledgement, req.RemediationJournalID)
	if err != nil {
		h.certWatchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// WithCertificationObserver wires the watch's metrics into the manual sweep route, so the flags a person's sweep raises are counted
// beside those the schedule and the events raise. Without it nothing is recorded.
func (h *Handler) WithCertificationObserver(o certwatch.Observer) *Handler {
	h.cwObserver = o
	return h
}
