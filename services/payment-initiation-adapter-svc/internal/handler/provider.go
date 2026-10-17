package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/eventing/adapter"
	"zoiko.io/payment-initiation-adapter-svc/internal/domain"
	svcmiddleware "zoiko.io/payment-initiation-adapter-svc/internal/middleware"
	"zoiko.io/payment-initiation-adapter-svc/internal/providers"
)

// Platform-scope actions for provider profiles. Certification is separate from publication so the author of
// a profile is never the person who certifies it (the database enforces that too).
const (
	ProviderProfilePublish = "PAYMENT_PROVIDER_PROFILE_PUBLISH"
	ProviderProfileCertify = "PAYMENT_PROVIDER_PROFILE_CERTIFY"
)

var providerKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,79}$`)

// WithProviders enables live provider adapters (INT-07). Without it every attempt uses the legacy stub path.
func (h *Handler) WithProviders(e *providers.Engine) *Handler {
	h.engine = e
	return h
}

// RegisterProviderRoutes adds the INT-07 endpoints.
func RegisterProviderRoutes(r chi.Router, h *Handler) {
	r.Route("/bnk06/provider-profiles", func(r chi.Router) {
		r.Post("/{providerKey}", h.PublishProviderProfile)
		r.Get("/", h.ListProviderProfiles)
		r.Get("/{providerKey}", h.ListProviderProfiles)
		r.Post("/{providerKey}/{version}/certify", h.CertifyProviderProfile)
		r.Post("/{providerKey}/{version}/deprecate", h.SetProviderProfileStatus(providers.ProfileDeprecated))
		r.Post("/{providerKey}/{version}/withdraw", h.SetProviderProfileStatus(providers.ProfileWithdrawn))
	})
	r.Route("/bnk06/attempts/{attemptID}/provider", func(r chi.Router) {
		r.Get("/", h.GetProviderDispatch)
		r.Get("/transport", h.GetProviderTransport)
		r.Post("/query", h.QueryProvider)
	})
}

func (h *Handler) providerStore() providers.Store {
	if h.engine == nil {
		return nil
	}
	return h.engine.Store
}

// exactAmount renders the request's amount as an exact decimal with at most two places (the column is
// NUMERIC(18,2)); anything finer would be silently rounded by the database, which is a change to the money.
func exactAmount(f float64) (string, bool) {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 && len(s)-i-1 > 2 {
		return "", false
	}
	if !strings.Contains(s, ".") {
		s += ".00"
	} else if len(s)-strings.IndexByte(s, '.')-1 == 1 {
		s += "0"
	}
	return s, true
}

// checkProviderBinding validates the optional provider fields of a prepare request. It refuses early what
// the provider profile can never carry.
func (h *Handler) checkProviderBinding(w http.ResponseWriter, r *http.Request, req *domain.PrepareAttemptRequest) bool {
	if req.ProviderKey == "" {
		if req.Rail != "" || req.ConnectorID != "" {
			writeError(w, http.StatusBadRequest, "rail and connector_id need a provider_key")
			return false
		}
		return true
	}
	if h.engine == nil {
		writeError(w, http.StatusUnprocessableEntity, "provider adapters are not enabled on this service")
		return false
	}
	if !providerKeyRe.MatchString(req.ProviderKey) || req.Rail == "" || req.ConnectorID == "" {
		writeError(w, http.StatusBadRequest, "provider_key, rail and connector_id are all required for a provider-bound attempt")
		return false
	}
	amount, ok := exactAmount(req.Amount)
	if !ok {
		writeError(w, http.StatusBadRequest, "amount must have at most two decimal places")
		return false
	}
	if err := h.engine.CheckBinding(r.Context(), req.ProviderKey, req.Rail, req.Currency, amount); err != nil {
		switch {
		case errors.Is(err, providers.ErrProfileNotFound):
			writeError(w, http.StatusUnprocessableEntity, "no certified provider profile for this provider_key")
		case errors.Is(err, providers.ErrProfileMismatch):
			writeError(w, http.StatusUnprocessableEntity, "the provider profile cannot carry this payment (rail, currency or limit)")
		default:
			h.log.Error("PrepareAttempt: provider profile lookup failed", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store unavailable")
		}
		return false
	}
	return true
}

// sendViaEngine handles a submit or retry for a provider-bound attempt. It returns false when the attempt
// is not provider-bound (the caller then uses the legacy stub path).
func (h *Handler) sendViaEngine(w http.ResponseWriter, r *http.Request, a *domain.PaymentInitiationAttempt, principalID string) bool {
	if h.engine == nil {
		return false
	}
	d, err := h.engine.Store.LoadDispatch(r.Context(), a.AttemptID)
	if err != nil {
		h.log.Error("sendViaEngine: dispatch lookup failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return true
	}
	if d.ProviderKey == "" {
		return false
	}
	res, err := h.engine.Send(r.Context(), a.AttemptID, principalID, h.caller(r, principalID))
	h.writeEngineResult(w, a, res, err)
	return true
}

func (h *Handler) caller(r *http.Request, principalID string) providers.Caller {
	return providers.Caller{TenantID: svcmiddleware.TenantFromContext(r.Context()), PrincipalID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID")}
}

func (h *Handler) writeEngineResult(w http.ResponseWriter, a *domain.PaymentInitiationAttempt, res *providers.Result, err error) {
	if res != nil && res.Note != "" {
		w.Header().Set("X-Adapter-Note", res.Note)
	}
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res.Attempt)
	case errors.Is(err, providers.ErrUnresolved):
		// not an error of the request: the attempt honestly remains pending
		writeJSON(w, http.StatusAccepted, map[string]any{"attempt_id": a.AttemptID, "status": domain.StatusPendingUnknown, "note": noteOr(res, "unresolved")})
	case errors.Is(err, providers.ErrInFlight), errors.Is(err, providers.ErrPayloadChanged):
		writeError(w, http.StatusConflict, "attempt is being sent, has changed state, or its payload differs from the one first sent")
	case errors.Is(err, providers.ErrNotBefore):
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "the provider asked to be called later")
	case errors.Is(err, providers.ErrCircuitOpen):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusServiceUnavailable, "the provider is failing; sending is paused and nothing was sent")
	case errors.Is(err, providers.ErrConnectorRefused):
		writeError(w, http.StatusUnprocessableEntity, ruleOf(err))
	case errors.Is(err, providers.ErrProfileNotFound), errors.Is(err, providers.ErrProfileMismatch), errors.Is(err, providers.ErrNotConfigured):
		writeError(w, http.StatusUnprocessableEntity, ruleOf(err))
	case errors.Is(err, providers.ErrCredentialRefused):
		writeError(w, http.StatusUnprocessableEntity, "the vault refused the provider credential; nothing was sent")
	case errors.Is(err, providers.ErrConnectorDown), errors.Is(err, providers.ErrCredentialDown):
		writeError(w, http.StatusServiceUnavailable, "a dependency is unavailable; nothing was sent")
	case errors.Is(err, domain.ErrAttemptNotFound):
		writeError(w, http.StatusNotFound, "payment initiation attempt not found")
	default:
		h.log.Error("provider send failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
	}
}

func noteOr(res *providers.Result, def string) string {
	if res != nil && res.Note != "" {
		return res.Note
	}
	return def
}

// ruleOf returns the rule-bearing message of an engine error (they never carry provider or credential content).
func ruleOf(err error) string { return err.Error() }

// ── dispatch evidence ───────────────────────────────────────────────────────

func (h *Handler) GetProviderDispatch(w http.ResponseWriter, r *http.Request) {
	st := h.providerStore()
	if st == nil {
		writeError(w, http.StatusNotFound, "provider adapters are not enabled on this service")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAttemptForAuth(w, r, chi.URLParam(r, "attemptID"))
	if !ok || !h.authorize(w, r, principalID, a.LegalEntityID, PaymentAttemptRead) {
		return
	}
	d, err := st.LoadDispatch(r.Context(), a.AttemptID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) GetProviderTransport(w http.ResponseWriter, r *http.Request) {
	st := h.providerStore()
	if st == nil {
		writeError(w, http.StatusNotFound, "provider adapters are not enabled on this service")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAttemptForAuth(w, r, chi.URLParam(r, "attemptID"))
	if !ok || !h.authorize(w, r, principalID, a.LegalEntityID, PaymentAttemptRead) {
		return
	}
	entries, err := st.ListTransport(r.Context(), a.AttemptID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	if entries == nil {
		entries = []providers.TransportEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempt_id": a.AttemptID, "calls": entries})
}

// QueryProvider asks the provider what it has for a PENDING_UNKNOWN attempt. It never sends the payment.
func (h *Handler) QueryProvider(w http.ResponseWriter, r *http.Request) {
	if h.engine == nil {
		writeError(w, http.StatusNotFound, "provider adapters are not enabled on this service")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	a, ok := h.fetchAttemptForAuth(w, r, chi.URLParam(r, "attemptID"))
	if !ok || !h.authorize(w, r, principalID, a.LegalEntityID, PaymentAttemptResolve) {
		return
	}
	res, err := h.engine.Query(r.Context(), a.AttemptID, principalID, h.caller(r, principalID))
	if err == nil && res.Attempt == nil {
		w.Header().Set("X-Adapter-Note", res.Note)
		writeJSON(w, http.StatusOK, map[string]any{"attempt_id": a.AttemptID, "status": domain.StatusPendingUnknown, "note": res.Note})
		return
	}
	h.writeEngineResult(w, a, res, err)
}

// ── profiles (platform scope) ───────────────────────────────────────────────

func (h *Handler) PublishProviderProfile(w http.ResponseWriter, r *http.Request) {
	st := h.providerStore()
	if st == nil {
		writeError(w, http.StatusNotFound, "provider adapters are not enabled on this service")
		return
	}
	key := chi.URLParam(r, "providerKey")
	if !providerKeyRe.MatchString(key) {
		writeError(w, http.StatusBadRequest, "provider_key must be 3-80 lowercase letters, digits, dot, underscore or dash")
		return
	}
	var spec adapter.Spec
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
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
	if spec.Kind != "BANK" {
		writeError(w, http.StatusUnprocessableEntity, "this service publishes BANK profiles only")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok || !h.authorize(w, r, principalID, providers.PlatformScopeID, ProviderProfilePublish) {
		return
	}
	p, err := st.PutProfile(r.Context(), key, spec, principalID)
	if err != nil {
		h.writeProfileError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) ListProviderProfiles(w http.ResponseWriter, r *http.Request) {
	st := h.providerStore()
	if st == nil {
		writeError(w, http.StatusNotFound, "provider adapters are not enabled on this service")
		return
	}
	list, err := st.ListProfiles(r.Context(), chi.URLParam(r, "providerKey"))
	if err != nil {
		h.writeProfileError(w, err)
		return
	}
	if list == nil {
		list = []providers.ProfileRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": list})
}

func (h *Handler) CertifyProviderProfile(w http.ResponseWriter, r *http.Request) {
	st := h.providerStore()
	if st == nil {
		writeError(w, http.StatusNotFound, "provider adapters are not enabled on this service")
		return
	}
	version, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || version < 1 {
		writeError(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok || !h.authorize(w, r, principalID, providers.PlatformScopeID, ProviderProfileCertify) {
		return
	}
	p, err := st.CertifyProfile(r.Context(), chi.URLParam(r, "providerKey"), version, principalID)
	if err != nil {
		h.writeProfileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) SetProviderProfileStatus(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := h.providerStore()
		if st == nil {
			writeError(w, http.StatusNotFound, "provider adapters are not enabled on this service")
			return
		}
		version, err := strconv.Atoi(chi.URLParam(r, "version"))
		if err != nil || version < 1 {
			writeError(w, http.StatusBadRequest, "version must be a positive integer")
			return
		}
		principalID, ok := h.requirePrincipal(w, r)
		if !ok || !h.authorize(w, r, principalID, providers.PlatformScopeID, ProviderProfileCertify) {
			return
		}
		p, err := st.SetProfileStatus(r.Context(), chi.URLParam(r, "providerKey"), version, to)
		if err != nil {
			h.writeProfileError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func (h *Handler) writeProfileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, providers.ErrProfileNotFound):
		writeError(w, http.StatusNotFound, "provider profile not found")
	case errors.Is(err, providers.ErrProfileSelfCertify):
		writeError(w, http.StatusForbidden, providers.ErrProfileSelfCertify.Error())
	case errors.Is(err, providers.ErrProfileState), errors.Is(err, providers.ErrProfileExists):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.log.Error("provider profile store failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
	}
}
