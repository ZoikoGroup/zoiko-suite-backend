package handler

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/eventing/adapter"
	"zoiko.io/eventing/adapter/ext"
	"zoiko.io/tax-authority-interface-svc/internal/authority"
	"zoiko.io/tax-authority-interface-svc/internal/authz"
	"zoiko.io/tax-authority-interface-svc/internal/domain"
	"zoiko.io/tax-authority-interface-svc/internal/middleware"
)

// Actions for the authority adapter. Profile actions are checked at the platform scope; certification is separate
// from publication so the author of a profile is never the person who certifies it (the database enforces that too).
const (
	ActionFilingRead     = "TAX_FILING_READ"
	ActionProfilePublish = "TAX_AUTHORITY_PROFILE_PUBLISH"
	ActionProfileCertify = "TAX_AUTHORITY_PROFILE_CERTIFY"
	maxFilingBody        = 4 << 20
	headerCorrelation    = "X-Correlation-ID"
)

var providerKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,79}$`)

// WithAuthority enables the live authority adapter (INT-08). Without it every filing keeps the legacy behaviour:
// recorded honestly as PENDING, never transmitted.
func (h *Handler) WithAuthority(e *authority.Engine, cb *authority.CallbackProcessor, cbStore authority.CallbackStore) *Handler {
	h.engine, h.callbacks, h.cbStore = e, cb, cbStore
	return h
}

// registerAuthorityRoutes adds the INT-08 endpoints that carry a tenant, onto the /v1/tax-authority router.
func (h *Handler) registerAuthorityRoutes(r chi.Router) {
	{
		r.Get("/filings/{id}", h.GetFiling)
		r.Post("/filings/{id}/validate", h.ValidateFiling)
		r.Post("/filings/{id}/submit", h.SubmitFiling)
		r.Post("/filings/{id}/poll", h.PollFiling)
		r.Get("/filings/{id}/evidence", h.GetFilingEvidence)
		r.Get("/filings/{id}/receipt", h.GetFilingReceipt)
		r.Get("/callbacks", h.ListCallbacks)
		r.Get("/health", h.GetAuthorityHealth)
		r.Route("/provider-profiles", func(r chi.Router) {
			r.Post("/{providerKey}", h.PublishProfile)
			r.Get("/", h.ListProfiles)
			r.Get("/{providerKey}", h.ListProfiles)
			r.Post("/{providerKey}/{version}/certify", h.CertifyProfile)
			r.Post("/{providerKey}/{version}/deprecate", h.SetProfileStatus(authority.ProfileDeprecated))
			r.Post("/{providerKey}/{version}/withdraw", h.SetProfileStatus(authority.ProfileWithdrawn))
		})
	}
}

// RegisterCallbackRoute adds the authority callback receiver. It sits OUTSIDE the tenant-context group: it is
// machine-authenticated, and its signature (verified with the secret of the tenant and provider in the URL) is the
// authentication.
func RegisterCallbackRoute(r chi.Router, h *Handler) {
	r.Post("/v1/tax-authority/callbacks/{provider}/{tenantID}", h.AuthorityCallback)
}

func (h *Handler) caller(r *http.Request, principalID string) ext.Caller {
	return ext.Caller{TenantID: middleware.GetTenantID(r.Context()), PrincipalID: principalID, CorrelationID: r.Header.Get(headerCorrelation)}
}

// exactDecimal renders a float64 amount with at most four places, the column's scale.
func exactDecimal(f float64) (string, bool) {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return "", false
	}
	if scaled := new(big.Rat).Mul(r, big.NewRat(10000, 1)); !scaled.IsInt() {
		return "", false
	}
	return r.FloatString(4), true
}

// submitViaAuthority records an approved filing subject for an authority-bound interface. It returns false when
// the interface is not bound to an authority (the caller then keeps the legacy behaviour).
func (h *Handler) submitViaAuthority(w http.ResponseWriter, r *http.Request, iface *domain.TaxInterface, req domain.SubmitTaxFilingRequest, principalID string) bool {
	if h.engine == nil || iface.ProviderKey == "" {
		return false
	}
	if len(req.Document) == 0 || req.SubjectRef == "" || req.SubjectFingerprint == "" || req.Rail == "" {
		writeError(w, http.StatusBadRequest, "an authority-bound interface needs document, subject_ref, subject_fingerprint and rail")
		return true
	}
	hash, err := adapter.CanonicalHash(req.Document)
	if err != nil {
		writeError(w, http.StatusBadRequest, "document must be a JSON object")
		return true
	}
	if hash != req.SubjectFingerprint {
		// the document is not the one that was approved: it is refused, never adapted to fit
		writeError(w, http.StatusUnprocessableEntity, "subject_fingerprint does not match the document: payload changed after approval")
		return true
	}
	amount, ok := exactDecimal(req.TaxAmount)
	if !ok {
		writeError(w, http.StatusBadRequest, "tax_amount must be non-negative with at most four decimal places")
		return true
	}
	if err := h.engine.CheckBinding(r.Context(), iface.ProviderKey, req.Rail); err != nil {
		switch {
		case errors.Is(err, authority.ErrProfileNotFound):
			writeError(w, http.StatusUnprocessableEntity, "no certified authority profile for this interface")
		case errors.Is(err, authority.ErrBlocked):
			writeError(w, http.StatusUnprocessableEntity, "the authority profile does not support this schema version")
		default:
			h.logger.Error("SubmitTaxFiling: profile lookup failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return true
	}
	f, created, err := h.engine.Store.CreateFiling(r.Context(), authority.CreateFiling{
		InterfaceID: iface.InterfaceID, TaxPeriod: req.TaxPeriod, FilingType: req.FilingType, AmountText: amount,
		SubjectRef: req.SubjectRef, SubjectFingerprint: req.SubjectFingerprint, Document: req.Document, PayloadHash: hash, Rail: req.Rail,
		DeadlineAt: req.DeadlineAt, Actor: principalID, CorrelationID: r.Header.Get(headerCorrelation),
	})
	switch {
	case errors.Is(err, authority.ErrSubjectConflict):
		writeError(w, http.StatusConflict, authority.ErrSubjectConflict.Error())
	case err != nil:
		h.logger.Error("SubmitTaxFiling: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
	case created:
		writeJSON(w, http.StatusCreated, f)
	default:
		writeJSON(w, http.StatusOK, f) // the same approved subject again: the one existing attempt
	}
	return true
}

func (h *Handler) noAuthority(w http.ResponseWriter) bool {
	if h.engine == nil {
		writeError(w, http.StatusNotFound, "the authority adapter is not enabled on this service")
		return true
	}
	return false
}

// loadFilingFor loads a filing and authorizes the principal for it.
func (h *Handler) loadFilingFor(w http.ResponseWriter, r *http.Request, action string) (*authority.Filing, string, bool) {
	if h.noAuthority(w) {
		return nil, "", false
	}
	principalID := r.Header.Get(principalIDHeader)
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "missing X-Principal-Id header")
		return nil, "", false
	}
	f, err := h.engine.Store.LoadFiling(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeEngineError(w, nil, err)
		return nil, "", false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, f.LegalEntityID, action); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized for this filing")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization check failed")
		}
		return nil, "", false
	}
	return f, principalID, true
}

func (h *Handler) GetFiling(w http.ResponseWriter, r *http.Request) {
	f, _, ok := h.loadFilingFor(w, r, ActionFilingRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (h *Handler) ValidateFiling(w http.ResponseWriter, r *http.Request) {
	f, principalID, ok := h.loadFilingFor(w, r, ActionFilingSubmit)
	if !ok {
		return
	}
	res, err := h.engine.Validate(r.Context(), f.SubmissionID, principalID, r.Header.Get(headerCorrelation))
	h.writeEngineResult(w, res, err)
}

func (h *Handler) SubmitFiling(w http.ResponseWriter, r *http.Request) {
	f, principalID, ok := h.loadFilingFor(w, r, ActionFilingSubmit)
	if !ok {
		return
	}
	res, err := h.engine.Send(r.Context(), f.SubmissionID, principalID, h.caller(r, principalID))
	h.writeEngineResult(w, res, err)
}

func (h *Handler) PollFiling(w http.ResponseWriter, r *http.Request) {
	f, principalID, ok := h.loadFilingFor(w, r, ActionFilingSubmit)
	if !ok {
		return
	}
	res, err := h.engine.Poll(r.Context(), f.SubmissionID, principalID, h.caller(r, principalID))
	h.writeEngineResult(w, res, err)
}

func (h *Handler) GetFilingEvidence(w http.ResponseWriter, r *http.Request) {
	f, _, ok := h.loadFilingFor(w, r, ActionFilingRead)
	if !ok {
		return
	}
	calls, err := h.engine.Store.ListTransport(r.Context(), f.SubmissionID)
	if err != nil {
		h.writeEngineError(w, nil, err)
		return
	}
	if calls == nil {
		calls = []authority.TransportEntry{}
	}
	var callbacks []authority.CallbackReceipt
	if h.cbStore != nil {
		callbacks, _ = h.cbStore.ListCallbacks(r.Context(), f.SubmissionID, 100)
	}
	if callbacks == nil {
		callbacks = []authority.CallbackReceipt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"submission_id": f.SubmissionID, "payload_hash": f.PayloadHash, "sent_payload_hash": f.SentPayloadHash,
		"rail": f.Rail, "profile_version": f.ProfileVersion, "certificate_ref": f.CertificateRef,
		"calls": calls, "callbacks": callbacks,
	})
}

func (h *Handler) GetFilingReceipt(w http.ResponseWriter, r *http.Request) {
	f, _, ok := h.loadFilingFor(w, r, ActionFilingRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"submission_id": f.SubmissionID, "status": f.Status, "authority_ref": f.AuthorityRef,
		"authority_status": f.AuthorityStatus, "receipt_hash": f.ReceiptHash, "last_failure": f.LastFailure,
		"accepted": f.Status == authority.StatusAccepted,
	})
}

func (h *Handler) ListCallbacks(w http.ResponseWriter, r *http.Request) {
	if h.noAuthority(w) || h.cbStore == nil {
		if h.cbStore == nil && h.engine != nil {
			writeError(w, http.StatusNotFound, "authority callbacks are not enabled on this service")
		}
		return
	}
	principalID := r.Header.Get(principalIDHeader)
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "missing X-Principal-Id header")
		return
	}
	id := r.URL.Query().Get("submission_id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "submission_id is required")
		return
	}
	f, err := h.engine.Store.LoadFiling(r.Context(), id)
	if err != nil {
		h.writeEngineError(w, nil, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, f.LegalEntityID, ActionFilingRead); err != nil {
		writeError(w, http.StatusForbidden, "not authorized for this filing")
		return
	}
	list, err := h.cbStore.ListCallbacks(r.Context(), id, 100)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if list == nil {
		list = []authority.CallbackReceipt{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"callbacks": list})
}

// GetAuthorityHealth reports this tenant's recent submit calls to an authority and whether sending is paused.
func (h *Handler) GetAuthorityHealth(w http.ResponseWriter, r *http.Request) {
	if h.noAuthority(w) {
		return
	}
	key := r.URL.Query().Get("provider_key")
	if !providerKeyRe.MatchString(key) {
		writeError(w, http.StatusBadRequest, "provider_key is required")
		return
	}
	calls, err := h.engine.Store.RecentCalls(r.Context(), key, time.Now().Add(-time.Hour), 20)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	failures := 0
	for _, ok := range calls {
		if !ok {
			failures++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider_key": key, "recent_calls": len(calls), "recent_failures": failures, "window": "1h"})
}

// AuthorityCallback is POST /v1/tax-authority/callbacks/{provider}/{tenantID}.
func (h *Handler) AuthorityCallback(w http.ResponseWriter, r *http.Request) {
	if h.callbacks == nil {
		writeError(w, http.StatusNotFound, "authority callbacks are not enabled on this service")
		return
	}
	res := h.callbacks.Handle(r.Context(), chi.URLParam(r, "provider"), chi.URLParam(r, "tenantID"), r.Header, http.MaxBytesReader(w, r.Body, 256<<10))
	if res.Status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "30")
	}
	writeJSON(w, res.Status, res.Body)
}

// ── engine results ──────────────────────────────────────────────────────────

func (h *Handler) writeEngineResult(w http.ResponseWriter, res *authority.Result, err error) {
	if res != nil && res.Note != "" && err == nil {
		w.Header().Set("X-Adapter-Note", res.Note)
	}
	if err == nil {
		writeJSON(w, http.StatusOK, res.Filing)
		return
	}
	h.writeEngineError(w, res, err)
}

func (h *Handler) writeEngineError(w http.ResponseWriter, res *authority.Result, err error) {
	switch {
	case errors.Is(err, authority.ErrUnresolved):
		note := "unresolved"
		if res != nil && res.Note != "" {
			note = res.Note
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"status": authority.StatusPendingUnknown, "note": note})
	case errors.Is(err, authority.ErrBlocked):
		body := map[string]any{"error": "the filing does not satisfy the authority schema or profile", "status": authority.StatusBlocked}
		if res != nil {
			body["detail"] = res.Note // names paths and rules, never values
		}
		writeJSON(w, http.StatusUnprocessableEntity, body)
	case errors.Is(err, authority.ErrCertificateExpired):
		writeError(w, http.StatusUnprocessableEntity, "the taxpayer certificate has expired; nothing was sent")
	case errors.Is(err, authority.ErrFilingNotFound):
		writeError(w, http.StatusNotFound, "tax filing submission not found")
	case errors.Is(err, authority.ErrInFlight), errors.Is(err, authority.ErrPayloadChanged):
		writeError(w, http.StatusConflict, "the filing is being sent, has changed state, or its payload differs from the one first sent")
	case errors.Is(err, authority.ErrNotBefore):
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "the authority asked to be called later")
	case errors.Is(err, authority.ErrCircuitOpen):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusServiceUnavailable, "the authority is failing; sending is paused and nothing was sent")
	case errors.Is(err, ext.ErrConnectorRefused), errors.Is(err, authority.ErrProfileNotFound), errors.Is(err, authority.ErrNotConfigured), errors.Is(err, authority.ErrNoQuery):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ext.ErrCredentialRefused):
		writeError(w, http.StatusUnprocessableEntity, "the vault refused the authority credential; nothing was sent")
	case errors.Is(err, ext.ErrConnectorDown), errors.Is(err, ext.ErrCredentialDown):
		writeError(w, http.StatusServiceUnavailable, "a dependency is unavailable; nothing was sent")
	default:
		h.logger.Error("authority operation failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
	}
}

// ── profiles (platform scope) ───────────────────────────────────────────────

func (h *Handler) PublishProfile(w http.ResponseWriter, r *http.Request) {
	if h.noAuthority(w) {
		return
	}
	key := chi.URLParam(r, "providerKey")
	if !providerKeyRe.MatchString(key) {
		writeError(w, http.StatusBadRequest, "provider_key must be 3-80 lowercase letters, digits, dot, underscore or dash")
		return
	}
	var spec adapter.Spec
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFilingBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid profile body")
		return
	}
	spec = spec.Normalize()
	if err := spec.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if spec.Kind != "TAX" {
		writeError(w, http.StatusUnprocessableEntity, "this service publishes TAX profiles only")
		return
	}
	principalID := r.Header.Get(principalIDHeader)
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "missing X-Principal-Id header")
		return
	}
	if !h.platformAllowed(w, r, principalID, ActionProfilePublish) {
		return
	}
	p, err := h.engine.Store.PutProfile(r.Context(), key, spec, principalID)
	if err != nil {
		h.writeProfileError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) platformAllowed(w http.ResponseWriter, r *http.Request, principalID, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, authority.PlatformScopeID, action); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized at the platform scope")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization check failed")
		}
		return false
	}
	return true
}

func (h *Handler) ListProfiles(w http.ResponseWriter, r *http.Request) {
	if h.noAuthority(w) {
		return
	}
	list, err := h.engine.Store.ListProfiles(r.Context(), chi.URLParam(r, "providerKey"))
	if err != nil {
		h.writeProfileError(w, err)
		return
	}
	if list == nil {
		list = []authority.ProfileRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": list})
}

func (h *Handler) CertifyProfile(w http.ResponseWriter, r *http.Request) {
	if h.noAuthority(w) {
		return
	}
	version, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || version < 1 {
		writeError(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	principalID := r.Header.Get(principalIDHeader)
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "missing X-Principal-Id header")
		return
	}
	if !h.platformAllowed(w, r, principalID, ActionProfileCertify) {
		return
	}
	p, err := h.engine.Store.CertifyProfile(r.Context(), chi.URLParam(r, "providerKey"), version, principalID)
	if err != nil {
		h.writeProfileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) SetProfileStatus(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.noAuthority(w) {
			return
		}
		version, err := strconv.Atoi(chi.URLParam(r, "version"))
		if err != nil || version < 1 {
			writeError(w, http.StatusBadRequest, "version must be a positive integer")
			return
		}
		principalID := r.Header.Get(principalIDHeader)
		if principalID == "" {
			writeError(w, http.StatusUnauthorized, "missing X-Principal-Id header")
			return
		}
		if !h.platformAllowed(w, r, principalID, ActionProfileCertify) {
			return
		}
		p, err := h.engine.Store.SetProfileStatus(r.Context(), chi.URLParam(r, "providerKey"), version, to)
		if err != nil {
			h.writeProfileError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func (h *Handler) writeProfileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, authority.ErrProfileNotFound):
		writeError(w, http.StatusNotFound, "authority profile not found")
	case errors.Is(err, authority.ErrProfileSelfCertify):
		writeError(w, http.StatusForbidden, authority.ErrProfileSelfCertify.Error())
	case errors.Is(err, authority.ErrProfileState), errors.Is(err, authority.ErrProfileExists):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("authority profile store failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
	}
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateAuthorityBinding checks the optional authority fields of an interface. A provider needs a connector;
// a connector, certificate or escalation window without a provider is a mistake, not a setting.
func validateAuthorityBinding(req domain.CreateInterfaceRequest) string {
	if req.ProviderKey == "" {
		if req.ConnectorID != "" || req.CertificateRef != "" || req.CertificateNotAfter != nil || req.EscalateBeforeHours != 0 {
			return "connector_id, certificate and escalation settings need a provider_key"
		}
		return ""
	}
	if !providerKeyRe.MatchString(req.ProviderKey) {
		return "provider_key must be 3-80 lowercase letters, digits, dot, underscore or dash"
	}
	if !uuidRe.MatchString(req.ConnectorID) {
		return "connector_id must be the id of an approved connector"
	}
	if req.EscalateBeforeHours < 0 || req.EscalateBeforeHours > 720 {
		return "escalate_before_hours must be between 1 and 720"
	}
	return ""
}
