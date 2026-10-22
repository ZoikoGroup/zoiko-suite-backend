package idenv

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ProblemBase is the type URI prefix of ZoikoSuite problem documents
// (ZS-API-001 §12).
const ProblemBase = "https://api.zoikosuite.com/problems/"

// ContentTypeProblem is RFC 9457's media type.
const ContentTypeProblem = "application/problem+json"

// Problem is an RFC 9457 problem document with ZS-API-001 §12's extensions.
type Problem struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Instance      string `json:"instance,omitempty"`
	Code          string `json:"code"`
	RequestID     string `json:"request_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	Retryable     bool   `json:"retryable"`
	// Error repeats Code under the key the console's readErrorDetail and the
	// pre-9457 clients branch on, until they read code.
	Error string `json:"error,omitempty"`
	// Extra members (e.g. required_auth_age_seconds) are merged in.
	Extra map[string]any `json:"-"`
}

// WriteProblem renders p. Type defaults to ProblemBase+code with
// underscores as hyphens, and request/correlation ids come from the request.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Type == "" {
		p.Type = ProblemBase + strings.ReplaceAll(p.Code, "_", "-")
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	if p.Error == "" {
		p.Error = p.Code
	}
	if r != nil {
		if p.RequestID == "" {
			p.RequestID = RequestIDFrom(r)
		}
		if p.CorrelationID == "" {
			p.CorrelationID = firstNonEmpty(r.Header.Get("Zoiko-Correlation-Id"), r.Header.Get("X-Correlation-ID"))
		}
	}
	if p.Instance == "" && p.RequestID != "" {
		p.Instance = "urn:zoikosuite:problem:" + p.RequestID
	}
	body := map[string]any{}
	raw, _ := json.Marshal(p)
	_ = json.Unmarshal(raw, &body)
	for k, v := range p.Extra {
		if _, taken := body[k]; !taken {
			body[k] = v
		}
	}
	w.Header().Set("Content-Type", ContentTypeProblem)
	w.Header().Set("Cache-Control", "no-store")
	if p.RequestID != "" {
		w.Header().Set(HeaderZoikoRequestID, p.RequestID)
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(body)
}

// HeaderZoikoRequestID is ZS-API-001 §5's request identifier header.
const HeaderZoikoRequestID = "Zoiko-Request-Id"

// HeaderXRequestID is the header the estate used before (still echoed).
const HeaderXRequestID = "X-Request-Id"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._:\-]{8,128}$`)

type reqIDKey struct{}

// RequestID adopts a valid client Zoiko-Request-Id (or legacy X-Request-Id),
// otherwise mints req_<ULID-like>, and echoes it on both headers of the
// response and the request (AU-24). Mount it first.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(HeaderZoikoRequestID))
		if !validRequestID.MatchString(id) {
			id = strings.TrimSpace(r.Header.Get(HeaderXRequestID))
		}
		if !validRequestID.MatchString(id) {
			id = NewRequestID()
		}
		r.Header.Set(HeaderZoikoRequestID, id)
		r.Header.Set(HeaderXRequestID, id)
		w.Header().Set(HeaderZoikoRequestID, id)
		w.Header().Set(HeaderXRequestID, id)
		next.ServeHTTP(w, r)
	})
}

// RequestIDFrom returns the request's id (after RequestID ran).
func RequestIDFrom(r *http.Request) string {
	return firstNonEmpty(r.Header.Get(HeaderZoikoRequestID), r.Header.Get(HeaderXRequestID))
}

// NewRequestID mints a sortable request id: req_ + 12 hex digits of
// millisecond time + 20 random hex digits.
func NewRequestID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("req_%012x%x", time.Now().UnixMilli(), b)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// unverifiedClaims decodes a token's claims without checking anything, for
// log context only.
func unverifiedClaims(token string) *Claims {
	c := &Claims{}
	_, _, _ = jwt.NewParser().ParseUnverified(token, c)
	return c
}
