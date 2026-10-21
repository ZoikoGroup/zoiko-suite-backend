package whout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/eventing/egress"
	"zoiko.io/eventing/integration"
)

// Errors. Messages name the rule and never echo caller input.
var (
	ErrNotFound         = errors.New("webhook subscription not found")
	ErrDeliveryNotFound = errors.New("webhook delivery not found")
	ErrVersionConflict  = errors.New("webhook subscription was changed by someone else; re-read it and retry")
	ErrInvalidState     = errors.New("that action is not allowed in the subscription's current state")
	ErrSelfApproval     = errors.New("a subscription cannot be approved by the person who last changed it")
	ErrNotApproved      = errors.New("the subscription configuration is not the approved configuration")
	ErrValidation       = errors.New("invalid webhook subscription")
	ErrRestrictedField  = errors.New("a restricted field cannot be sent to a webhook destination")
	ErrOutsideScope     = errors.New("the payload contains a field outside the approved subscription scope")
	ErrReplayNotAllowed = errors.New("only a dead-lettered delivery can be replayed")
	ErrReplayUnapproved = errors.New("a replay needs an operator, a different approver, an approval reference and a reason")
	ErrEgressDenied     = errors.New("the destination is not an allowed egress target")
	ErrKeyUnavailable   = errors.New("the signing key reference could not be confirmed")
	ErrDependency       = errors.New("a dependency needed to check this change is unavailable; retry")
)

var (
	eventTypePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+)*(\.\*)?$`)
	fieldPattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*){0,2}$`)
	keyPathPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/_.-]{2,198}$`)
	regionPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}$`)
)

// restrictedFragments are lower-cased substrings of a field name (separators removed)
// that mark a field as one a webhook may never carry (INT-CTRL-011). They are
// refused when a subscription is configured AND checked again on every body before
// it is sent, so a payload that gains such a field later is still stopped.
var restrictedFragments = []string{
	"password", "passwd", "passcode", "secret", "token", "apikey", "authorization", "privatekey",
	"credential", "ssn", "nationalid", "taxid", "accountnumber", "cardnumber", "iban", "cvv", "cvc",
	"routingnumber", "sortcode", "bearer", "pincode",
}

// restrictedWords are whole words that are only restricted as a complete name
// segment ("pin" must not catch "shipping").
var restrictedWords = map[string]bool{"pin": true, "otp": true, "pan": true}

// IsRestrictedField reports whether any segment of a (dotted) field path names a
// restricted value.
func IsRestrictedField(path string) bool {
	for _, seg := range strings.Split(path, ".") {
		low := strings.ToLower(seg)
		for _, w := range strings.FieldsFunc(low, func(r rune) bool { return r == '_' || r == '-' }) {
			if restrictedWords[w] {
				return true
			}
		}
		norm := strings.NewReplacer("_", "", "-", "").Replace(low)
		for _, f := range restrictedFragments {
			if strings.Contains(norm, f) {
				return true
			}
		}
	}
	return false
}

// MatchEventType reports whether an event type is selected by a subscription's
// filters. A filter is an exact type or a prefix ending ".*" ("invoice.*" selects
// "invoice.paid" and "invoice.line.added"). A bare "*" is not a valid filter.
func MatchEventType(filters []string, eventType string) bool {
	for _, f := range filters {
		if f == eventType {
			return true
		}
		if strings.HasSuffix(f, ".*") && strings.HasPrefix(eventType, strings.TrimSuffix(f, "*")) {
			return true
		}
	}
	return false
}

// Spec is the input that defines a subscription's configuration.
type Spec struct {
	LegalEntityID     string
	Name              string
	DestinationURL    string
	EventTypes        []string
	AllowedFields     []string
	APIVersion        string
	SigningKeyPath    string
	DestinationRegion string
	MaxAttempts       int
}

// Normalize returns a copy with defaults applied and lists de-duplicated and sorted,
// so equal configurations always have equal fingerprints.
func (s Spec) Normalize() Spec {
	s.Name = strings.TrimSpace(s.Name)
	s.DestinationURL = strings.TrimSpace(s.DestinationURL)
	s.SigningKeyPath = strings.TrimSpace(s.SigningKeyPath)
	s.DestinationRegion = strings.ToLower(strings.TrimSpace(s.DestinationRegion))
	if s.APIVersion == "" {
		s.APIVersion = "v1"
	}
	if s.MaxAttempts == 0 {
		s.MaxAttempts = 8
	}
	s.EventTypes = sortedUnique(s.EventTypes)
	s.AllowedFields = sortedUnique(s.AllowedFields)
	return s
}

func sortedUnique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// Validate checks everything that can be checked without the network or the vault.
// The destination's address and the signing key reference are verified by the
// service, which has those dependencies.
func (s Spec) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrValidation, msg) }
	if s.LegalEntityID == "" {
		return bad("legal_entity_id is required")
	}
	if s.Name == "" || len(s.Name) > 200 {
		return bad("name is required (at most 200 characters)")
	}
	if s.DestinationURL == "" {
		return bad("destination_url is required")
	}
	if strings.ContainsAny(s.DestinationURL, "@?#") {
		return bad("destination_url must not carry credentials, a query string or a fragment")
	}
	if len(s.EventTypes) == 0 || len(s.EventTypes) > 50 {
		return bad("between 1 and 50 event types are required")
	}
	for _, t := range s.EventTypes {
		if len(t) > 120 || !eventTypePattern.MatchString(t) {
			return bad("an event type is malformed (a bare * is not allowed; use a prefix such as invoice.*)")
		}
	}
	if len(s.AllowedFields) > 100 {
		return bad("at most 100 allowed fields")
	}
	for _, f := range s.AllowedFields {
		if len(f) > 120 || !fieldPattern.MatchString(f) {
			return bad("an allowed field is malformed (dotted path, at most 3 levels)")
		}
		if IsRestrictedField(f) {
			return fmt.Errorf("%w: %s", ErrRestrictedField, "an allowed field names restricted data")
		}
	}
	if s.APIVersion != "v1" {
		return bad("api_version is not supported")
	}
	if !keyPathPattern.MatchString(s.SigningKeyPath) {
		return bad("signing_key_path is not a valid secret path")
	}
	if !regionPattern.MatchString(s.DestinationRegion) {
		return bad("destination_region is required")
	}
	if s.MaxAttempts < 1 || s.MaxAttempts > 20 {
		return bad("max_attempts must be between 1 and 20")
	}
	return nil
}

// Fingerprint is SHA-256 over everything an approver is approving: where events go,
// which events, which fields, which API version and which region. The signing key
// reference is deliberately NOT part of it: rotating a key must not stop deliveries
// for want of a fresh approval, and the vault itself only signs for the owning tenant
// and only with a signing-class secret. Name and retry budget are likewise free to
// change without a second person.
func (s Spec) Fingerprint() string {
	n := s.Normalize()
	canon, _ := json.Marshal(struct {
		URL     string   `json:"u"`
		Events  []string `json:"e"`
		Fields  []string `json:"f"`
		Version string   `json:"v"`
		Region  string   `json:"r"`
		Entity  string   `json:"l"`
	}{n.DestinationURL, n.EventTypes, n.AllowedFields, n.APIVersion, n.DestinationRegion, n.LegalEntityID})
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// ScopeFingerprint identifies the field scope a body was minimized to.
func ScopeFingerprint(fields []string) string {
	canon, _ := json.Marshal(sortedUnique(fields))
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// SpecOf returns the configuration of a stored subscription.
func SpecOf(s *Subscription) Spec {
	return Spec{
		LegalEntityID: s.LegalEntityID, Name: s.Name, DestinationURL: s.DestinationURL, EventTypes: s.EventTypes,
		AllowedFields: s.AllowedFields, APIVersion: s.APIVersion, SigningKeyPath: s.SigningKeyPath,
		DestinationRegion: s.DestinationRegion, MaxAttempts: s.MaxAttempts,
	}.Normalize()
}

// deliveryNamespace is the UUIDv5 namespace for delivery identities.
var deliveryNamespace = uuid.MustParse("6f1d6f0e-5b0a-4f55-9a52-3d0c6a1f2b77")

// DeliveryID is stable for one (subscription, event): a redelivered event maps to the
// same delivery, and every retry and replay keeps it (INT-04 idempotency).
func DeliveryID(subscriptionID, eventID string) string {
	return uuid.NewSHA1(deliveryNamespace, []byte(subscriptionID+"|"+eventID)).String()
}

// SourceEvent is the part of a platform event envelope a webhook is built from.
type SourceEvent struct {
	EventID    string
	EventType  string
	TenantID   string
	OccurredAt time.Time
	Payload    map[string]any
}

// ParseSource decodes the platform event envelope. The envelope variants in the
// estate name the timestamp emitted_at or occurred_at; either is accepted.
func ParseSource(raw []byte) (SourceEvent, error) {
	var w struct {
		EventID    string          `json:"event_id"`
		EventType  string          `json:"event_type"`
		TenantID   string          `json:"tenant_id"`
		EmittedAt  time.Time       `json:"emitted_at"`
		OccurredAt time.Time       `json:"occurred_at"`
		Payload    json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return SourceEvent{}, fmt.Errorf("event is not JSON: %w", err)
	}
	if w.EventID == "" || w.EventType == "" || w.TenantID == "" {
		return SourceEvent{}, errors.New("event lacks event_id, event_type or tenant_id")
	}
	ev := SourceEvent{EventID: w.EventID, EventType: w.EventType, TenantID: w.TenantID, OccurredAt: w.OccurredAt}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = w.EmittedAt
	}
	if ev.OccurredAt.IsZero() {
		return SourceEvent{}, errors.New("event has no timestamp")
	}
	if len(w.Payload) > 0 && string(w.Payload) != "null" {
		if err := json.Unmarshal(w.Payload, &ev.Payload); err != nil {
			return SourceEvent{}, errors.New("event payload is not a JSON object")
		}
	}
	return ev, nil
}

// MaxBodyBytes bounds a webhook body (the vault signs at most ~180 KiB).
const MaxBodyBytes = 64 << 10

// envelopeKeys are the top-level members every webhook body carries.
var envelopeKeys = map[string]bool{"id": true, "type": true, "created": true, "api_version": true, "tenant_id": true, "data": true}

// Minimize builds the webhook body: a fixed envelope plus ONLY the payload fields the
// subscription allows. Nothing outside allowedFields is copied, however the source
// event is shaped (INT-CTRL-011).
func Minimize(ev SourceEvent, allowedFields []string, apiVersion string) ([]byte, error) {
	data := map[string]any{}
	for _, path := range allowedFields {
		if IsRestrictedField(path) {
			return nil, ErrRestrictedField
		}
		if v, ok := lookup(ev.Payload, strings.Split(path, ".")); ok {
			setPath(data, strings.Split(path, "."), v)
		}
	}
	body, err := json.Marshal(map[string]any{
		"id": ev.EventID, "type": ev.EventType, "created": ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		"api_version": apiVersion, "tenant_id": ev.TenantID, "data": data,
	})
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("%w: webhook body exceeds %d bytes", ErrValidation, MaxBodyBytes)
	}
	return body, nil
}

func lookup(m map[string]any, path []string) (any, bool) {
	cur := any(m)
	for _, seg := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func setPath(dst map[string]any, path []string, v any) {
	for _, seg := range path[:len(path)-1] {
		next, ok := dst[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			dst[seg] = next
		}
		dst = next
	}
	dst[path[len(path)-1]] = v
}

// VerifyBody is the pre-send gate. Given the body exactly as it will be sent and the
// subscription's CURRENT approved scope it refuses (a) any top-level member outside
// the fixed envelope, (b) any data field outside the allowed scope (the scope may have
// narrowed since the body was built), (c) any restricted field name anywhere in the
// data, and (d) any value that looks like a secret. A refusal is a control failure
// (INT-NP-017), not a delivery to retry.
func VerifyBody(body []byte, allowedFields []string) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return fmt.Errorf("%w: body is not a JSON object", ErrOutsideScope)
	}
	for k := range top {
		if !envelopeKeys[k] {
			return ErrOutsideScope
		}
	}
	var data map[string]any
	if raw, ok := top["data"]; ok {
		if err := json.Unmarshal(raw, &data); err != nil {
			return ErrOutsideScope
		}
	}
	allowed := make(map[string]bool, len(allowedFields))
	for _, f := range allowedFields {
		allowed[f] = true
	}
	if err := walkData(data, "", allowed); err != nil {
		return err
	}
	if integration.ScrubString(string(body)) != string(body) {
		return ErrRestrictedField
	}
	return nil
}

// walkData requires every leaf path of the data to be an allowed field (or inside
// the subtree of an allowed field that names an object).
func walkData(m map[string]any, prefix string, allowed map[string]bool) error {
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if IsRestrictedField(path) {
			return ErrRestrictedField
		}
		if allowed[path] {
			if sub, ok := v.(map[string]any); ok {
				if err := walkRestricted(sub, path); err != nil {
					return err
				}
			}
			continue
		}
		sub, ok := v.(map[string]any)
		if !ok || !hasAllowedDescendant(path, allowed) {
			return ErrOutsideScope
		}
		if err := walkData(sub, path, allowed); err != nil {
			return err
		}
	}
	return nil
}

func walkRestricted(m map[string]any, prefix string) error {
	for k, v := range m {
		path := prefix + "." + k
		if IsRestrictedField(path) {
			return ErrRestrictedField
		}
		if sub, ok := v.(map[string]any); ok {
			if err := walkRestricted(sub, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasAllowedDescendant(path string, allowed map[string]bool) bool {
	for a := range allowed {
		if strings.HasPrefix(a, path+".") {
			return true
		}
	}
	return false
}

// RetryPolicy bounds retry (INT-CTRL-012): exponential backoff with full jitter, so a
// destination that recovers is not hit by every queued delivery at the same instant.
type RetryPolicy struct {
	Base time.Duration
	Cap  time.Duration
}

// DefaultRetry is a sound starting point: 30s, 1m, 2m, 4m ... capped at an hour.
var DefaultRetry = RetryPolicy{Base: 30 * time.Second, Cap: time.Hour}

// Delay is the wait before attempt attemptsMade+1. jitter returns a value in [0,1).
func (p RetryPolicy) Delay(attemptsMade int, jitter func() float64) time.Duration {
	if attemptsMade < 1 {
		attemptsMade = 1
	}
	ceil := float64(p.Base) * math.Pow(2, float64(attemptsMade-1))
	if ceil > float64(p.Cap) || math.IsInf(ceil, 0) {
		ceil = float64(p.Cap)
	}
	d := time.Duration(ceil * (0.5 + 0.5*jitter())) // between half and all of the ceiling
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Result classifies one HTTP exchange.
type Result struct {
	Outcome   AttemptOutcome
	Retryable bool
	// Permanent means do not retry: the destination said it is gone.
	Permanent  bool
	ErrorClass string
	RetryAfter time.Duration
}

// ClassifyStatus maps a response status to a Result. Any non-2xx is a failed
// delivery that goes through bounded retry; 410 Gone ends the delivery at once.
func ClassifyStatus(code int, retryAfter time.Duration) Result {
	switch {
	case code >= 200 && code < 300:
		return Result{Outcome: OutcomeSucceeded}
	case code == http.StatusTooManyRequests:
		return Result{Outcome: OutcomeRateLimited, Retryable: true, ErrorClass: "http_429", RetryAfter: retryAfter}
	case code == http.StatusGone:
		return Result{Outcome: OutcomeHTTPError, Permanent: true, ErrorClass: "http_410"}
	case code >= 300 && code < 400:
		// a redirect has not passed egress policy; it is never followed
		return Result{Outcome: OutcomeHTTPError, Retryable: true, ErrorClass: "redirect_not_followed"}
	case code >= 500:
		return Result{Outcome: OutcomeHTTPError, Retryable: true, ErrorClass: "http_5xx", RetryAfter: retryAfter}
	default:
		return Result{Outcome: OutcomeHTTPError, Retryable: true, ErrorClass: "http_4xx"}
	}
}

func hostOnly(raw string) string { return egress.HostOf(raw) }
