package actionlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
)

// ActionTokenStore defines the store operations required by the Action Gateway.
type ActionTokenStore interface {
	GetActionTokenByHash(ctx context.Context, tenantID, tokenHash string) (*ledger.ActionToken, error)
	ConsumeActionToken(ctx context.Context, tenantID, tokenHash, clientIP string) (*ledger.ActionToken, error)
}

// Gateway implements the Link-Scanner-Safe Action Gateway per ZS-COMMS-EMAIL-001 §6.
// Invariant: GET requests from automated link scanners display read-only confirmation and NEVER mutate state.
// Only confirmed POST requests atomically consume the single-use token.
type Gateway struct {
	store   ActionTokenStore
	signer  *Signer
	log     *zap.Logger
	landing *template.Template
	errorPg *template.Template
	donePg  *template.Template
}

// NewGateway initializes the Action Gateway.
func NewGateway(store ActionTokenStore, signer *Signer, log *zap.Logger) (*Gateway, error) {
	if store == nil {
		return nil, errors.New("missing action token store")
	}
	if signer == nil {
		return nil, errors.New("missing action link signer")
	}
	if log == nil {
		log = zap.NewNop()
	}

	landingTmpl, err := template.New("landing").Parse(landingPageHTML)
	if err != nil {
		return nil, fmt.Errorf("parse landing template: %w", err)
	}

	errorTmpl, err := template.New("error").Parse(errorPageHTML)
	if err != nil {
		return nil, fmt.Errorf("parse error template: %w", err)
	}

	doneTmpl, err := template.New("done").Parse(successPageHTML)
	if err != nil {
		return nil, fmt.Errorf("parse success template: %w", err)
	}

	return &Gateway{
		store:   store,
		signer:  signer,
		log:     log,
		landing: landingTmpl,
		errorPg: errorTmpl,
		donePg:  doneTmpl,
	}, nil
}

// RegisterRoutes mounts the action link gateway routes onto a Chi router.
func (g *Gateway) RegisterRoutes(r chi.Router) {
	r.Get("/v1/notifications/actions/{token}", g.HandleLandingPage)
	r.Post("/v1/notifications/actions/{token}/execute", g.HandleExecute)
}

// HandleLandingPage serves a read-only confirmation page on GET.
// Automated mail scanners (Outlook SafeLinks, Barracuda, etc.) crawling this URL trigger ZERO state changes.
func (g *Gateway) HandleLandingPage(w http.ResponseWriter, r *http.Request) {
	tokenStr := chi.URLParam(r, "token")
	if tokenStr == "" {
		tokenStr = strings.TrimPrefix(r.URL.Path, "/v1/notifications/actions/")
		tokenStr = strings.TrimSuffix(tokenStr, "/execute")
	}

	tenantID, purpose, _, tokenHash, err := g.signer.VerifyAndExtract(tokenStr)
	if err != nil {
		g.log.Warn("action gateway: invalid token signature on landing page", zap.Error(err))
		g.renderError(w, r, http.StatusBadRequest, "Invalid Action Link", "The action link is invalid or has an invalid cryptographic signature.")
		return
	}

	tok, err := g.store.GetActionTokenByHash(r.Context(), tenantID, tokenHash)
	if err != nil {
		if errors.Is(err, store.ErrActionTokenNotFound) {
			g.renderError(w, r, http.StatusNotFound, "Action Link Not Found", "The requested action link does not exist or has already been purged.")
			return
		}
		g.log.Error("action gateway: store error on get token", zap.Error(err))
		g.renderError(w, r, http.StatusInternalServerError, "Service Unavailable", "A temporary error occurred while retrieving the action link.")
		return
	}

	if tok.Status == ledger.ActionTokenStatusConsumed {
		g.renderError(w, r, http.StatusGone, "Action Already Completed", "This single-use action has already been completed and cannot be repeated.")
		return
	}
	if tok.Status == ledger.ActionTokenStatusRevoked {
		g.renderError(w, r, http.StatusGone, "Action Link Revoked", "This action link has been revoked by an administrator or system policy.")
		return
	}
	if tok.Status == ledger.ActionTokenStatusExpired || time.Now().UTC().After(tok.ExpiresAt) {
		g.renderError(w, r, http.StatusGone, "Action Link Expired", "This action link has expired. Please request a new link.")
		return
	}

	// Link-Scanner Safety: Read-only inspection only. Zero database mutations!
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":                string(tok.Status),
			"purpose":               tok.Purpose,
			"requires_confirmation": true,
			"expires_at":            tok.ExpiresAt.Format(time.RFC3339),
			"execute_url":           fmt.Sprintf("/v1/notifications/actions/%s/execute", tokenStr),
		})
		return
	}

	data := struct {
		Title       string
		Purpose     string
		PurposeName string
		Token       string
		ExecuteURL  string
		ExpiresAt   string
	}{
		Title:       "Confirm Action - ZoikoSuite",
		Purpose:     purpose,
		PurposeName: humanizePurpose(purpose),
		Token:       tokenStr,
		ExecuteURL:  fmt.Sprintf("/v1/notifications/actions/%s/execute", tokenStr),
		ExpiresAt:   tok.ExpiresAt.Format("Jan 02, 2006 15:04 UTC"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = g.landing.Execute(w, data)
}

// HandleExecute atomically consumes the token on confirmed POST and redirects/responds.
func (g *Gateway) HandleExecute(w http.ResponseWriter, r *http.Request) {
	tokenStr := chi.URLParam(r, "token")
	if tokenStr == "" {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 4 && parts[len(parts)-1] == "execute" {
			tokenStr = parts[len(parts)-2]
		}
	}

	tenantID, purpose, _, tokenHash, err := g.signer.VerifyAndExtract(tokenStr)
	if err != nil {
		g.log.Warn("action gateway: invalid token signature on execute", zap.Error(err))
		g.renderError(w, r, http.StatusBadRequest, "Invalid Action Link", "The action link is invalid or has an invalid cryptographic signature.")
		return
	}

	clientIP := extractClientIP(r)

	consumedTok, err := g.store.ConsumeActionToken(r.Context(), tenantID, tokenHash, clientIP)
	if err != nil {
		if errors.Is(err, store.ErrActionTokenNotFound) {
			g.renderError(w, r, http.StatusNotFound, "Action Link Not Found", "The requested action link was not found.")
			return
		}
		if errors.Is(err, store.ErrActionTokenAlreadyConsumed) {
			g.renderError(w, r, http.StatusConflict, "Action Already Completed", "This single-use action has already been completed.")
			return
		}
		if errors.Is(err, store.ErrActionTokenExpired) {
			g.renderError(w, r, http.StatusGone, "Action Link Expired", "This action link has expired.")
			return
		}
		if errors.Is(err, store.ErrActionTokenRevoked) {
			g.renderError(w, r, http.StatusGone, "Action Link Revoked", "This action link has been revoked.")
			return
		}
		g.log.Error("action gateway: failed to consume token", zap.Error(err), zap.String("tenant_id", tenantID))
		g.renderError(w, r, http.StatusInternalServerError, "Execution Failed", "Failed to process action token. Please try again.")
		return
	}

	g.log.Info("action gateway: action token consumed successfully",
		zap.String("token_id", consumedTok.TokenID),
		zap.String("purpose", purpose),
		zap.String("tenant_id", tenantID),
		zap.String("client_ip", clientIP),
	)

	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":            "CONSUMED",
			"purpose":           consumedTok.Purpose,
			"target_action_url": consumedTok.TargetActionURL,
			"target_method":     consumedTok.TargetMethod,
		})
		return
	}

	if consumedTok.TargetActionURL != "" && (strings.HasPrefix(consumedTok.TargetActionURL, "http://") || strings.HasPrefix(consumedTok.TargetActionURL, "https://") || strings.HasPrefix(consumedTok.TargetActionURL, "/")) {
		http.Redirect(w, r, consumedTok.TargetActionURL, http.StatusSeeOther)
		return
	}

	data := struct {
		Title       string
		PurposeName string
	}{
		Title:       "Action Confirmed - ZoikoSuite",
		PurposeName: humanizePurpose(consumedTok.Purpose),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = g.donePg.Execute(w, data)
}

func (g *Gateway) renderError(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   title,
			"message": message,
		})
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = g.errorPg.Execute(w, struct {
		Title   string
		Heading string
		Message string
	}{
		Title:   title + " - ZoikoSuite",
		Heading: title,
		Message: message,
	})
}

func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") || r.URL.Query().Get("format") == "json"
}

func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func humanizePurpose(purpose string) string {
	switch purpose {
	case "VERIFY_EMAIL":
		return "Verify Email Address"
	case "RESET_PASSWORD":
		return "Reset Account Password"
	case "ONE_TIME_SIGN_IN":
		return "Secure Sign-In"
	case "WORKSPACE_INVITE":
		return "Join Organization Workspace"
	case "CONFIRM_EMAIL_CHANGE":
		return "Confirm New Email Address"
	default:
		return strings.Title(strings.ToLower(strings.ReplaceAll(purpose, "_", " ")))
	}
}

const landingPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{.Title}}</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background-color: #0b0f19; color: #f3f4f6; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; padding: 1rem; }
        .card { background-color: #111827; border: 1px solid #1f2937; border-radius: 12px; padding: 2.5rem; max-width: 480px; width: 100%; box-shadow: 0 20px 25px -5px rgba(0, 0, 0, 0.5); text-align: center; }
        .logo { font-size: 1.5rem; font-weight: 700; letter-spacing: -0.025em; color: #6366f1; margin-bottom: 1.5rem; }
        h1 { font-size: 1.25rem; font-weight: 600; margin-bottom: 0.75rem; color: #ffffff; }
        p { font-size: 0.95rem; color: #9ca3af; line-height: 1.5; margin-bottom: 1.75rem; }
        .btn { display: inline-block; width: 100%; padding: 0.875rem 1.5rem; background-color: #4f46e5; color: #ffffff; font-weight: 600; font-size: 0.95rem; border: none; border-radius: 8px; cursor: pointer; transition: background-color 0.15s ease; text-decoration: none; box-sizing: border-box; }
        .btn:hover { background-color: #4338ca; }
        .footer { margin-top: 1.5rem; font-size: 0.8rem; color: #6b7280; }
    </style>
</head>
<body>
    <div class="card">
        <div class="logo">ZoikoSuite</div>
        <h1>Confirm Action</h1>
        <p>Please click below to complete your requested action:<br><strong>{{.PurposeName}}</strong></p>
        <form method="POST" action="{{.ExecuteURL}}">
            <button type="submit" class="btn" id="confirm-action-button">Confirm &amp; Complete Action</button>
        </form>
        <div class="footer">Link expires at {{.ExpiresAt}}. This confirmation protects your account against automated link scanners.</div>
    </div>
</body>
</html>`

const errorPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{.Title}}</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background-color: #0b0f19; color: #f3f4f6; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; padding: 1rem; }
        .card { background-color: #111827; border: 1px solid #1f2937; border-radius: 12px; padding: 2.5rem; max-width: 480px; width: 100%; box-shadow: 0 20px 25px -5px rgba(0, 0, 0, 0.5); text-align: center; }
        .logo { font-size: 1.5rem; font-weight: 700; letter-spacing: -0.025em; color: #ef4444; margin-bottom: 1.5rem; }
        h1 { font-size: 1.25rem; font-weight: 600; margin-bottom: 0.75rem; color: #ffffff; }
        p { font-size: 0.95rem; color: #9ca3af; line-height: 1.5; margin-bottom: 1.5rem; }
    </style>
</head>
<body>
    <div class="card">
        <div class="logo">ZoikoSuite</div>
        <h1>{{.Heading}}</h1>
        <p>{{.Message}}</p>
    </div>
</body>
</html>`

const successPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{.Title}}</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background-color: #0b0f19; color: #f3f4f6; display: flex; align-items: center; justify-content: center; min-height: 100vh; margin: 0; padding: 1rem; }
        .card { background-color: #111827; border: 1px solid #1f2937; border-radius: 12px; padding: 2.5rem; max-width: 480px; width: 100%; box-shadow: 0 20px 25px -5px rgba(0, 0, 0, 0.5); text-align: center; }
        .logo { font-size: 1.5rem; font-weight: 700; letter-spacing: -0.025em; color: #10b981; margin-bottom: 1.5rem; }
        h1 { font-size: 1.25rem; font-weight: 600; margin-bottom: 0.75rem; color: #ffffff; }
        p { font-size: 0.95rem; color: #9ca3af; line-height: 1.5; margin-bottom: 1.5rem; }
    </style>
</head>
<body>
    <div class="card">
        <div class="logo">ZoikoSuite</div>
        <h1>Action Confirmed</h1>
        <p>Your action <strong>{{.PurposeName}}</strong> has been successfully processed.</p>
    </div>
</body>
</html>`
