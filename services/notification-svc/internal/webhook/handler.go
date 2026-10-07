package webhook

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Handler handles incoming ESP webhook HTTP requests.
type Handler struct {
	processor *Processor
	verifier  *Verifier
	log       *zap.Logger
}

// WithVerifier sets the callback authenticator. Without one the handler refuses
// every callback (503): an unauthenticated state-changing ingress is never the
// default (ZS-SVC-Y-001 INV-27).
func (h *Handler) WithVerifier(v *Verifier) *Handler {
	h.verifier = v
	return h
}

func NewHandler(processor *Processor, log *zap.Logger) *Handler {
	if log == nil {
		log = zap.NewNop()
	}
	return &Handler{
		processor: processor,
		log:       log,
	}
}

// RegisterRoutes mounts the webhook endpoint onto a Chi router.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Post("/v1/notifications/webhooks/{provider}", h.HandleWebhook)
}

// HandleWebhook processes an incoming provider webhook.
func (h *Handler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	if strings.TrimSpace(provider) == "" {
		provider = "generic"
	}

	const maxBody = 2 * 1024 * 1024 // 2MB limit
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		h.log.Warn("failed to read webhook body", zap.Error(err))
		http.Error(w, `{"error":"unable to read body"}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	if len(body) > maxBody {
		http.Error(w, `{"error":"webhook payload too large"}`, http.StatusRequestEntityTooLarge)
		return
	}

	// Authenticate before anything is parsed, recorded or routed to the DLQ: a
	// forged callback must not change delivery state, create a suppression or
	// even occupy the DLQ (INV-27, NP-26). Every failure is the same 401 so the
	// response does not say which part was wrong; the reason is logged.
	if h.verifier == nil {
		h.log.Error("webhook refused: no verifier configured; ingress is closed", zap.String("provider", provider))
		http.Error(w, `{"status":"error","message":"webhook ingress is not configured"}`, http.StatusServiceUnavailable)
		return
	}
	if err := h.verifier.Verify(provider, r.Header.Get(SignatureHeader), body); err != nil {
		h.log.Warn("webhook refused: signature verification failed",
			zap.String("provider", provider),
			zap.String("reason", err.Error()),
			zap.String("remote_addr", r.RemoteAddr),
		)
		if h.processor != nil && h.processor.metrics != nil {
			h.processor.metrics.RecordWebhookEvent(provider, "signature_rejected")
		}
		http.Error(w, `{"status":"error","message":"invalid webhook signature"}`, http.StatusUnauthorized)
		return
	}

	if len(body) == 0 {
		http.Error(w, `{"error":"empty webhook payload"}`, http.StatusBadRequest)
		return
	}

	if err := h.processor.ProcessRawPayload(r.Context(), provider, body); err != nil {
		h.log.Warn("webhook processing reported failure",
			zap.String("provider", provider),
			zap.Error(err),
		)
		// For transient database or internal failures, return 500 so ESP retries.
		// For permanently invalid payloads, DLQ has already captured the item, return 400.
		if strings.Contains(err.Error(), "invalid webhook") {
			http.Error(w, `{"status":"error","message":"invalid webhook event recorded in dlq"}`, http.StatusBadRequest)
			return
		}
		http.Error(w, `{"status":"error","message":"transient processing error, please retry"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","message":"webhook processed"}`))
}
