// Package contract defines the public wire contract of /public-ai/v1
// (Doc 05 §08–§10, §15; Doc 04 §10) and its validation rules.
package contract

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	SchemaVersion   = "1.0"
	MaxMessageRunes = 2000
	MaxTextRunes    = 5000
	MaxCitations    = 5 // schema bound (Doc 05 §10); the orchestrator emits ≤3 (ADR-005)
	MaxActions      = 3 // schema bound; the orchestrator emits ≤1 primary CTA (ADR-005)
	ActionOpenRoute = "OPEN_OFFICIAL_ROUTE"
)

// Status is the turn outcome (Doc 05 §10). Business outcomes are HTTP 200.
type Status string

const (
	Answered      Status = "ANSWERED"
	Clarify       Status = "CLARIFY"
	RefusedAction Status = "REFUSED_ACTION"
	Abstain       Status = "ABSTAIN"
	Handoff       Status = "HANDOFF"
	Unavailable   Status = "UNAVAILABLE"
)

var validStatus = map[Status]bool{Answered: true, Clarify: true, RefusedAction: true, Abstain: true, Handoff: true, Unavailable: true}

// PageContexts is the allowlisted page_context enum (Doc 05 §08).
var PageContexts = map[string]bool{"home": true, "pricing": true, "core_modules": true, "demo": true, "support": true, "other": true}

var (
	marketRe = regexp.MustCompile(`^[A-Z]{2}$`)
	localeRe = regexp.MustCompile(`^[a-zA-Z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	intentRe = regexp.MustCompile(`^(I(0[1-9]|[12][0-9]|30)|X(0[1-9]|10)|UNKNOWN)$`)
)

// SessionRequest is the body of POST /sessions.
type SessionRequest struct {
	SchemaVersion string `json:"schema_version"`
	Locale        string `json:"locale,omitempty"`
	MarketCode    string `json:"market_code,omitempty"`
	PageContext   string `json:"page_context,omitempty"`
}

type Capabilities struct {
	Streaming     bool  `json:"streaming"`
	LeadCapture   bool  `json:"lead_capture"`
	FileUpload    bool  `json:"file_upload"`
	Authenticated *bool `json:"authenticated,omitempty"`
}

// SessionResponse is the 201 body of POST /sessions.
type SessionResponse struct {
	SchemaVersion string       `json:"schema_version"`
	SessionID     string       `json:"session_id"`
	SessionGrant  string       `json:"session_grant"`
	ExpiresAt     string       `json:"expires_at"`
	PolicyVersion string       `json:"policy_version"`
	Capabilities  Capabilities `json:"capabilities"`
}

// TurnRequest is the body of POST /turns.
type TurnRequest struct {
	SchemaVersion         string `json:"schema_version"`
	SessionID             string `json:"session_id"`
	ClientTurnID          string `json:"client_turn_id"`
	Message               string `json:"message"`
	Locale                string `json:"locale,omitempty"`
	MarketCode            string `json:"market_code,omitempty"`
	PageContext           string `json:"page_context,omitempty"`
	RequestedResponseMode string `json:"requested_response_mode,omitempty"`
}

type Citation struct {
	SourceVersionID string   `json:"source_version_id"`
	ClaimIDs        []string `json:"claim_ids"`
	URLKey          string   `json:"url_key"`
}

type Action struct {
	Type     string `json:"type"`
	RouteKey string `json:"route_key"`
}

type Flags struct {
	RequiresHuman bool `json:"requires_human,omitempty"`
}

// Envelope is the turn response (zs-public-answer-v1).
type Envelope struct {
	SchemaVersion      string     `json:"schema_version"`
	TurnID             string     `json:"turn_id"`
	RequestID          string     `json:"request_id"`
	TurnSequence       int        `json:"turn_sequence"`
	Status             Status     `json:"status"`
	IntentID           string     `json:"intent_id"`
	Text               string     `json:"text"`
	Citations          []Citation `json:"citations"`
	Actions            []Action   `json:"actions"`
	KnowledgeReleaseID string     `json:"knowledge_release_id"`
	PolicyVersion      string     `json:"policy_version"`
	Flags              *Flags     `json:"flags,omitempty"`
}

// ConfigResponse is the body of GET /config (Doc 05 §15).
type ConfigResponse struct {
	ConfigRevision       string       `json:"config_revision"`
	AssistantDisplayName string       `json:"assistant_display_name"`
	Capabilities         Capabilities `json:"capabilities"`
	SupportedLocales     []string     `json:"supported_locales"`
	MarketChoices        []string     `json:"market_choices"`
	PublicRoutes         []string     `json:"public_routes"`
	PrivacyNoticeKey     string       `json:"privacy_notice_key"`
}

// FieldError is a 422 validation failure (safe to show publicly).
type FieldError struct{ Field, Reason string }

func (e *FieldError) Error() string { return e.Field + ": " + e.Reason }

// ErrUnsupportedSchema means schema_version is not "1.0".
var ErrUnsupportedSchema = errors.New("unsupported schema_version")

// NormalizeMessage applies Unicode NFC, removes control characters other
// than newline and tab, and trims surrounding space (Doc 05 §09).
func NormalizeMessage(s string) string {
	s = norm.NFC.String(strings.ToValidUTF8(s, ""))
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func validateOptional(locale, market, page string) error {
	if locale != "" && !localeRe.MatchString(locale) {
		return &FieldError{"locale", "must be a BCP 47 tag"}
	}
	if market != "" && !marketRe.MatchString(market) {
		return &FieldError{"market_code", "must be an ISO 3166-1 alpha-2 code"}
	}
	if page != "" && !PageContexts[page] {
		return &FieldError{"page_context", "unsupported value"}
	}
	return nil
}

// Validate checks a session request.
func (r *SessionRequest) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return ErrUnsupportedSchema
	}
	return validateOptional(r.Locale, r.MarketCode, r.PageContext)
}

// ValidUUID reports whether s is a canonical UUID.
func ValidUUID(s string) bool { return uuidRe.MatchString(s) }

// Validate checks and normalizes a turn request in place.
func (r *TurnRequest) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return ErrUnsupportedSchema
	}
	if utf8.RuneCountInString(r.SessionID) < 12 {
		return &FieldError{"session_id", "invalid"}
	}
	if !ValidUUID(r.ClientTurnID) {
		return &FieldError{"client_turn_id", "must be a UUID"}
	}
	r.ClientTurnID = strings.ToLower(r.ClientTurnID)
	r.Message = NormalizeMessage(r.Message)
	if n := utf8.RuneCountInString(r.Message); n < 1 || n > MaxMessageRunes {
		return &FieldError{"message", fmt.Sprintf("must be 1-%d characters", MaxMessageRunes)}
	}
	if r.RequestedResponseMode != "" && r.RequestedResponseMode != "json" {
		return &FieldError{"requested_response_mode", "only json is supported"}
	}
	return validateOptional(r.Locale, r.MarketCode, r.PageContext)
}

// Validate enforces the response schema before anything reaches a client.
func (e *Envelope) Validate() error {
	switch {
	case e.SchemaVersion != SchemaVersion:
		return errors.New("schema_version")
	case !validStatus[e.Status]:
		return errors.New("status")
	case !intentRe.MatchString(e.IntentID):
		return errors.New("intent_id")
	case utf8.RuneCountInString(e.Text) > MaxTextRunes:
		return errors.New("text too long")
	case len(e.Citations) > MaxCitations:
		return errors.New("too many citations")
	case len(e.Actions) > MaxActions:
		return errors.New("too many actions")
	case e.KnowledgeReleaseID == "" || e.PolicyVersion == "":
		return errors.New("release/policy required")
	case e.TurnSequence < 1:
		return errors.New("turn_sequence")
	}
	for _, a := range e.Actions {
		if a.Type != ActionOpenRoute || a.RouteKey == "" {
			return errors.New("action")
		}
	}
	for _, c := range e.Citations {
		if c.SourceVersionID == "" || c.URLKey == "" {
			return errors.New("citation")
		}
	}
	if e.Status == Answered && len(e.Citations) == 0 {
		return errors.New("ANSWERED requires provenance")
	}
	return nil
}
