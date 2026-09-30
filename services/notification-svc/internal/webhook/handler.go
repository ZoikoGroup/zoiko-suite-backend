package webhook

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// Handler handles incoming ESP webhook HTTP requests.
type Handler struct {
	processor *Processor
	log       *zap.Logger
	// secretFor resolves a provider's signing secret (see auth.go).
	secretFor func(provider string) string
	now       func() time.Time
}

// SetSecretLookup replaces the secret source (tests, secret stores).
func (h *Handler) SetSecretLookup(f func(provider string) string) { h.secretFor = f }

func NewHandler(processor *Processor, log *zap.Logger) *Handler {
	if log == nil {
		log = zap.NewNop()
	}
	return &Handler{
		processor: processor,
		log:       log,
		secretFor: EnvSecret,
		now:       time.Now,
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

	body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024)) // 2MB limit
	if err != nil {
		h.log.Warn("failed to read webhook body", zap.Error(err))
		http.Error(w, `{"error":"unable to read body"}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if len(body) == 0 {
		http.Error(w, `{"error":"empty webhook payload"}`, http.StatusBadRequest)
		return
	}

	// Authenticate before anything is parsed or written (INV-27).
	if err := verify(h.secretFor(provider), r.Header.Get("X-Webhook-Timestamp"), r.Header.Get("X-Webhook-Signature"), body, h.now()); err != nil {
		h.log.Warn("webhook rejected", zap.String("provider", provider), zap.String("remote", r.RemoteAddr), zap.Error(err))
		status := http.StatusUnauthorized
		if errors.Is(err, errSecretUnset) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, `{"status":"error","message":"`+err.Error()+`"}`, status)
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
