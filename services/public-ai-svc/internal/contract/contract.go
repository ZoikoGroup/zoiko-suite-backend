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

// Answer classes (Master prompt §4.1, Doc 02 §12): how much of the answer is
// a governed fact. Every status maps to exactly one class.
const (
	VerifiedFact       = "VERIFIED_FACT"
	StatusQualified    = "STATUS_QUALIFIED"
	ProcessExplanation = "PROCESS_EXPLANATION"
	HandoffUnknown     = "HANDOFF_UNKNOWN"
)

var validClass = map[string]bool{VerifiedFact: true, StatusQualified: true, ProcessExplanation: true, HandoffUnknown: true}

// StatusLabels is the governed status vocabulary. A label is shown only when
// it comes from the signed claim registry; it is never inferred or upgraded.
var StatusLabels = map[string]bool{
	"Available": true, "Limited": true, "Partner-Supported": true, "Planned": true, "Not Available": true,
	"Unverified": true, "Current Architecture": true, "Implemented": true, "In Validation": true, "Readiness": true,
	"MARKET_DEPENDENT": true,
}

// OverviewOption is the clarification choice that skips the missing slot and
// asks for the general, non-market answer instead.
const OverviewOption = "GENERAL_OVERVIEW"

// DefaultClass is the class for a status when the orchestrator set none.
func DefaultClass(s Status) string {
	switch s {
	case Answered:
		return ProcessExplanation
	case Clarify:
		return StatusQualified
	default:
		return HandoffUnknown
	}
}

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

// Clarification is the one question a CLARIFY turn asks: the slot it fills
// and the allowed choices (market codes plus OverviewOption).
type Clarification struct {
	Slot     string   `json:"slot"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
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
	// AnswerClass, StatusLabel and MarketCode are the qualifier the UI shows
	// next to the answer; Clarification is set only on CLARIFY.
	AnswerClass   string         `json:"answer_class"`
	StatusLabel   string         `json:"status_label,omitempty"`
	MarketCode    string         `json:"market_code,omitempty"`
	Clarification *Clarification `json:"clarification,omitempty"`
}

// ConfigResponse is the body of GET /config (Doc 05 §15).
type ConfigResponse struct {
	ConfigRevision       string        `json:"config_revision"`
	AssistantDisplayName string        `json:"assistant_display_name"`
	Capabilities         Capabilities  `json:"capabilities"`
	SupportedLocales     []string      `json:"supported_locales"`
	MarketChoices        []string      `json:"market_choices"`
	PublicRoutes         []PublicRoute `json:"public_routes"`
	// CitationLinks resolves a citation url_key to the approved public page
	// it came from (server-side resolution, Doc 06 §10). Extension of Doc 05
	// §15 recorded in ADR-005.
	CitationLinks    []CitationLink `json:"citation_links"`
	PrivacyNoticeKey string         `json:"privacy_notice_key"`
	// Microcopy (ui.* keys) and Chips (starter questions by page_context)
	// let the widget render pre-approved wording only (Master prompt §6.4).
	Microcopy map[string]string `json:"microcopy"`
	Chips     map[string][]Chip `json:"chips"`
}

// Chip is one welcome-card starter: a label and the fixed message it sends.
type Chip struct {
	Label   string `json:"label"`
	Message string `json:"message"`
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
	if n := utf8.RuneCountInString(r.Message); n < 1 {
		return &FieldError{"message", "must not be empty"}
	} else if n > MaxMessageRunes {
		return &FieldError{"message", fmt.Sprintf("too long: at most %d characters", MaxMessageRunes)}
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
	case !validClass[e.AnswerClass]:
		return errors.New("answer_class")
	case e.StatusLabel != "" && !StatusLabels[e.StatusLabel]:
		return errors.New("status_label")
	case e.MarketCode != "" && !marketRe.MatchString(e.MarketCode):
		return errors.New("market_code")
	case e.Clarification != nil && e.Status != Clarify:
		return errors.New("clarification only on CLARIFY")
	case e.AnswerClass == HandoffUnknown && e.Status == Answered:
		return errors.New("ANSWERED cannot be HANDOFF_UNKNOWN")
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

// FeedbackRequest is the body of POST /turns/{turn_id}/feedback (Doc 01
// FR-09 "useful / not useful"; extension recorded in ADR-005). Enums only.
type FeedbackRequest struct {
	SchemaVersion string `json:"schema_version"`
	SessionID     string `json:"session_id"`
	Rating        string `json:"rating"`
	Reason        string `json:"reason,omitempty"`
}

var feedbackReasons = map[string]bool{"": true, "incorrect": true, "incomplete": true, "not_relevant": true, "unclear": true, "other": true}

// Validate checks a feedback request.
func (f *FeedbackRequest) Validate() error {
	if f.SchemaVersion != SchemaVersion {
		return ErrUnsupportedSchema
	}
	if f.Rating != "useful" && f.Rating != "not_useful" {
		return &FieldError{"rating", "must be useful or not_useful"}
	}
	if !feedbackReasons[f.Reason] || (f.Rating == "useful" && f.Reason != "") {
		return &FieldError{"reason", "unsupported value"}
	}
	return nil
}

// HandoffRequest is the body of POST /handoffs: a visitor opened an official
// route (Doc 05 §21 chat.handoff.clicked.v1). It never creates a case.
type HandoffRequest struct {
	SchemaVersion string `json:"schema_version"`
	SessionID     string `json:"session_id"`
	RouteKey      string `json:"route_key"`
}

// HandoffResponse returns the approved URL for the route.
type HandoffResponse struct {
	RouteKey string `json:"route_key"`
	URL      string `json:"url"`
}

// PublicRoute is an official destination the widget may open. URLs come only
// from the signed route registry, never from model output (Doc 05 §15).
type PublicRoute struct {
	RouteKey string `json:"route_key"`
	URL      string `json:"url"`
}

// CitationLink maps a citation url_key to an approved, allowlisted HTTPS page.
type CitationLink struct {
	URLKey string `json:"url_key"`
	URL    string `json:"url"`
	Title  string `json:"title"`
}
