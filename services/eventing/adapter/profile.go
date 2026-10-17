// Package adapter is the shared provider-adapter framework of ZS-SVC-M-001 (INT-07, INT-08, INT-09).
//
// A provider adapter translates a canonical, already-approved subject (a payment attempt, a tax filing, an
// e-invoice) into a provider's own API and maps the provider's answer back. Everything that makes that safe
// is here and is the same for every provider and every kind of adapter:
//
//   - a certified Profile (declarative: request mapping, response and status mapping, callback verification,
//     limits), so connecting a provider is registering a profile, not writing a client;
//   - a request builder that proves the money in the translated request is the canonical money;
//   - a transport that tells "provably not sent" from "possibly sent", because a timeout after the request left
//     is an UNKNOWN outcome and must never be retried blindly (INT-NP-032);
//   - response classification into acknowledged / rejected / not-processed / rate-limited / unknown;
//   - callback verification (HMAC schemes, replay window) and event extraction.
//
// It never decides business state. A provider acknowledging a request is not payment finality, a tax
// authority accepting a transport is not an accepted filing: the owning domain decides what an outcome means.
package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// Spec is the certified definition of how to talk to one provider for one kind of adapter.
type Spec struct {
	Kind              string    `json:"kind"`           // BANK, TAX, EINVOICE
	RequiredScope     string    `json:"required_scope"` // the connector scope that must be approved (INT-05)
	Rails             []string  `json:"rails"`          // supported rail / message-version identifiers
	Currencies        []string  `json:"currencies,omitempty"`
	MaxAmount         string    `json:"max_amount,omitempty"` // decimal; empty means no limit here
	Submit            Request   `json:"submit"`
	Query             *Query    `json:"query,omitempty"`
	Response          Response  `json:"response"`
	Auth              Auth      `json:"auth"`
	IdempotencyHeader string    `json:"idempotency_header,omitempty"`
	RetrySafe         bool      `json:"retry_safe"` // the provider honours the idempotency key: the same key resent cannot duplicate
	TimeoutSeconds    int       `json:"timeout_seconds"`
	Callback          *Callback `json:"callback,omitempty"`
	// Schema is the authority submission schema (a small JSON-Schema subset); the approved document is
	// validated against it BEFORE anything is sent. Required for TAX and EINVOICE profiles.
	Schema        json.RawMessage `json:"schema,omitempty"`
	SchemaVersion string          `json:"schema_version,omitempty"`
}

// Request describes the outbound call.
type Request struct {
	Method       string  `json:"method"`
	Path         string  `json:"path"` // appended to the connector's approved endpoint
	ContentType  string  `json:"content_type,omitempty"`
	Body         []Field `json:"body"`
	AmountField  string  `json:"amount_field"`  // dotted path of the amount in the body: re-checked after the body is built
	AmountFormat string  `json:"amount_format"` // decimal (string "10.50"), number (10.50), minor (integer 1050)
}

// Field builds one member of the request body.
type Field struct {
	Name   string `json:"name"`             // dotted for nesting: "debtor.account"
	From   string `json:"from"`             // canonical | const
	Source string `json:"source,omitempty"` // the canonical key
	Const  any    `json:"const,omitempty"`
}

// Query is how the original submission is looked up (FetchOriginalAttemptStatus).
type Query struct {
	Method        string   `json:"method"`
	Path          string   `json:"path"` // may contain {provider_ref} and {idempotency_key}
	NotFoundCodes []int    `json:"not_found_codes"`
	Response      Response `json:"response"`
}

// Response maps a provider's answer.
type Response struct {
	SuccessCodes []int             `json:"success_codes"`
	RefPath      string            `json:"ref_path,omitempty"`
	StatusPath   string            `json:"status_path,omitempty"`
	ReasonPath   string            `json:"reason_path,omitempty"`
	StatusMap    map[string]string `json:"status_map,omitempty"` // provider status -> ACKNOWLEDGED | REJECTED | PENDING
	// EchoPath names a field in the answer that must echo the reference we sent. An authority receipt that does
	// not echo it is a mismatch, never an acceptance.
	EchoPath string `json:"echo_path,omitempty"`
}

// Auth says how the adapter authenticates to the provider.
type Auth struct {
	Type            string `json:"type"` // none | bearer | api_key | hmac
	Header          string `json:"header,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	SignatureHeader string `json:"signature_header,omitempty"`
}

// Callback says how a provider's callbacks are verified and read.
type Callback struct {
	Scheme           string            `json:"scheme"` // hmac-sha256-hex | zoiko-v1
	Header           string            `json:"header,omitempty"`
	ToleranceSeconds int               `json:"tolerance_seconds,omitempty"`
	EventRefPath     string            `json:"event_ref_path"`
	PaymentRefPath   string            `json:"payment_ref_path"`
	StatusPath       string            `json:"status_path"`
	EventTimePath    string            `json:"event_time_path,omitempty"`
	StatusMap        map[string]string `json:"status_map"`
}

// Canonical classes a status map may produce.
const (
	StatusAcknowledged = "ACKNOWLEDGED"
	StatusRejected     = "REJECTED"
	StatusPending      = "PENDING"
	// StatusAccepted is the authority accepting a filing (or a network clearing a document). It exists for TAX
	// and EINVOICE profiles only: a bank acknowledgement is never finality, so a BANK profile cannot map to it.
	StatusAccepted = "ACCEPTED"
)

// Errors. Messages name the rule and never echo caller or provider content.
var (
	ErrSpecInvalid      = errors.New("invalid provider profile")
	ErrRailUnsupported  = errors.New("the rail or message version is not supported by this provider profile")
	ErrCurrency         = errors.New("the currency is not supported by this provider profile")
	ErrLimitExceeded    = errors.New("the amount exceeds this provider profile's limit")
	ErrAmountMismatch   = errors.New("the translated request does not carry the canonical amount")
	ErrCanonicalMissing = errors.New("a canonical value the profile needs is missing")
	ErrDocumentMissing  = errors.New("the profile embeds the approved document and none was supplied")
)

var (
	pathKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	headerRe   = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	decimalRe  = regexp.MustCompile(`^-?[0-9]{1,18}(\.[0-9]{1,6})?$`)
	railRe     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}(@[A-Za-z0-9._-]{1,20})?$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Normalize applies defaults.
func (s Spec) Normalize() Spec {
	if s.Submit.Method == "" {
		s.Submit.Method = "POST"
	}
	if s.Submit.ContentType == "" {
		s.Submit.ContentType = "application/json"
	}
	if s.Submit.AmountFormat == "" {
		s.Submit.AmountFormat = "decimal"
	}
	if len(s.Response.SuccessCodes) == 0 {
		s.Response.SuccessCodes = []int{200, 201, 202}
	}
	if s.TimeoutSeconds == 0 {
		s.TimeoutSeconds = 15
	}
	if s.Auth.Type == "" {
		s.Auth.Type = "none"
	}
	return s
}

// Validate checks a profile is complete and safe to certify.
func (s Spec) Validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrSpecInvalid, fmt.Sprintf(f, a...)) }
	if s.Kind != "BANK" && s.Kind != "TAX" && s.Kind != "EINVOICE" {
		return bad("kind must be BANK, TAX or EINVOICE")
	}
	if strings.TrimSpace(s.RequiredScope) == "" {
		return bad("required_scope is required")
	}
	if len(s.Rails) == 0 {
		return bad("at least one supported rail is required")
	}
	for _, r := range s.Rails {
		if !railRe.MatchString(r) {
			return bad("a rail identifier is malformed")
		}
	}
	for _, c := range s.Currencies {
		if !currencyRe.MatchString(c) {
			return bad("a currency must be an ISO 4217 code")
		}
	}
	if s.MaxAmount != "" {
		if _, ok := new(big.Rat).SetString(s.MaxAmount); !ok || !decimalRe.MatchString(s.MaxAmount) {
			return bad("max_amount is not a plain decimal")
		}
	}
	if err := validateRequest(s.Kind, s.Submit); err != nil {
		return err
	}
	if len(s.Schema) > 0 {
		if _, err := CompileSchema(s.Schema); err != nil {
			return err
		}
	} else if s.Kind != "BANK" {
		return bad("a %s profile needs the authority schema: nothing is sent unvalidated", s.Kind)
	}
	if err := validateResponse(s.Kind, s.Response, "response"); err != nil {
		return err
	}
	if q := s.Query; q != nil {
		if q.Method != "GET" && q.Method != "POST" {
			return bad("query.method must be GET or POST")
		}
		if err := validatePath(q.Path, true); err != nil {
			return err
		}
		if len(q.NotFoundCodes) == 0 {
			return bad("query.not_found_codes is required: it is what proves the provider never received the request")
		}
		for _, c := range q.NotFoundCodes {
			if c < 400 || c > 499 {
				return bad("not_found_codes must be 4xx")
			}
		}
		if err := validateResponse(s.Kind, q.Response, "query.response"); err != nil {
			return err
		}
	}
	switch s.Auth.Type {
	case "none":
	case "bearer", "api_key":
		if s.Auth.Type == "api_key" && !headerRe.MatchString(s.Auth.Header) {
			return bad("an api_key auth needs a header name")
		}
	case "hmac":
		if !headerRe.MatchString(s.Auth.SignatureHeader) {
			return bad("an hmac auth needs a signature header name")
		}
	default:
		return bad("auth.type must be none, bearer, api_key or hmac")
	}
	if s.IdempotencyHeader != "" && !headerRe.MatchString(s.IdempotencyHeader) {
		return bad("idempotency_header is malformed")
	}
	if s.RetrySafe && s.IdempotencyHeader == "" {
		return bad("retry_safe is a claim about the idempotency key: it needs an idempotency_header")
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 60 {
		return bad("timeout_seconds must be between 1 and 60")
	}
	if s.Callback != nil {
		if err := s.Callback.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validatePath(p string, allowVars bool) error {
	bad := func(m string) error { return fmt.Errorf("%w: %s", ErrSpecInvalid, m) }
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?#\\") || strings.Contains(p, "://") || strings.Contains(p, "..") || strings.Contains(p, "//") {
		return bad("a path must be absolute with no query, fragment or traversal")
	}
	rest := p
	if allowVars {
		rest = strings.NewReplacer("{provider_ref}", "", "{idempotency_key}", "").Replace(p)
	}
	if strings.ContainsAny(rest, "{}") {
		return bad("a path contains an unknown placeholder")
	}
	return nil
}

func validateRequest(kind string, r Request) error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrSpecInvalid, fmt.Sprintf(f, a...)) }
	if r.Method != "POST" && r.Method != "PUT" && r.Method != "PATCH" {
		return bad("submit.method must be POST, PUT or PATCH")
	}
	if err := validatePath(r.Path, false); err != nil {
		return err
	}
	if r.AmountFormat != "decimal" && r.AmountFormat != "number" && r.AmountFormat != "minor" {
		return bad("submit.amount_format must be decimal, number or minor")
	}
	if r.AmountField == "" && kind != "BANK" {
		// a document profile may carry no separately mapped amount
	} else if !pathKeyRe.MatchString(r.AmountField) {
		return bad("submit.amount_field is required: the amount in the built request is re-checked against the canonical amount")
	}
	if len(r.Body) == 0 || len(r.Body) > 200 {
		return bad("between 1 and 200 body fields are required")
	}
	seen := map[string]bool{}
	hasAmount, hasDocument := false, false
	for _, f := range r.Body {
		if !pathKeyRe.MatchString(f.Name) || seen[f.Name] {
			return bad("a body field name is malformed or repeated")
		}
		seen[f.Name] = true
		switch f.From {
		case "canonical":
			if strings.TrimSpace(f.Source) == "" {
				return bad("a canonical field needs a source")
			}
			if f.Source == "amount" {
				if r.AmountField == "" {
					return bad("a request that maps the amount must name amount_field")
				}
				hasAmount = true
				if f.Name != r.AmountField {
					return bad("the canonical amount must be carried in amount_field and nowhere else")
				}
			}
		case "const":
			if f.Const == nil {
				return bad("a const field needs a value")
			}
		case "document":
			if kind == "BANK" {
				return bad("a bank request does not embed a document")
			}
			if hasDocument {
				return bad("only one body field may embed the document")
			}
			hasDocument = true
		default:
			return bad("a body field's source must be canonical, const or document")
		}
	}
	if kind == "BANK" && !hasAmount {
		return bad("the request must carry the canonical amount")
	}
	if kind != "BANK" && !hasDocument {
		return bad("a %s request must embed the approved document", kind)
	}
	return nil
}

func validateResponse(kind string, r Response, where string) error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrSpecInvalid, fmt.Sprintf(f, a...)) }
	for _, c := range r.SuccessCodes {
		if c < 200 || c > 299 {
			return bad("%s.success_codes must be 2xx", where)
		}
	}
	for _, p := range []string{r.RefPath, r.StatusPath, r.ReasonPath, r.EchoPath} {
		if p != "" && !pathKeyRe.MatchString(p) {
			return bad("a %s path is malformed", where)
		}
	}
	for _, v := range r.StatusMap {
		if v != StatusAcknowledged && v != StatusRejected && v != StatusPending && !(v == StatusAccepted && kind != "BANK") {
			return bad("a status maps to ACKNOWLEDGED, REJECTED or PENDING")
		}
	}
	if r.StatusPath != "" && len(r.StatusMap) == 0 {
		return bad("%s.status_path needs a status_map", where)
	}
	return nil
}

// ── path access ─────────────────────────────────────────────────────────────

func getPath(v any, path string) (any, bool) {
	for _, seg := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return v, true
}

func setPath(dst map[string]any, path string, v any) {
	segs := strings.Split(path, ".")
	for _, s := range segs[:len(segs)-1] {
		next, ok := dst[s].(map[string]any)
		if !ok {
			next = map[string]any{}
			dst[s] = next
		}
		dst = next
	}
	dst[segs[len(segs)-1]] = v
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// ── building and proving the request ────────────────────────────────────────

// Canonical is the already-approved subject the adapter was handed. Values are strings; the amount is a
// plain decimal.
type Canonical map[string]string

// minorExponent is the number of fractional digits of a currency's minor unit.
func minorExponent(currency string) int {
	switch strings.ToUpper(currency) {
	case "JPY", "KRW", "VND", "CLP", "ISK", "UGX", "XAF", "XOF":
		return 0
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 3
	}
	return 2
}

// CheckAgainstProfile refuses a subject the profile cannot carry: an unsupported rail, a currency
// outside the list, an amount over the limit. Nothing has been built or sent.
func (s Spec) CheckAgainstProfile(c Canonical) error {
	rail := c["rail"]
	ok := false
	for _, r := range s.Rails {
		if r == rail {
			ok = true
		}
	}
	if !ok {
		return ErrRailUnsupported
	}
	if len(s.Currencies) > 0 && (s.Kind == "BANK" || c["currency"] != "") {
		found := false
		for _, cur := range s.Currencies {
			if cur == c["currency"] {
				found = true
			}
		}
		if !found {
			return ErrCurrency
		}
	}
	if s.Kind != "BANK" && c["amount"] == "" {
		return nil // a filing may carry no single amount
	}
	amt, aok := new(big.Rat).SetString(c["amount"])
	if !aok || !decimalRe.MatchString(c["amount"]) || amt.Sign() < 0 || (s.Kind == "BANK" && amt.Sign() == 0) {
		return fmt.Errorf("%w: amount", ErrCanonicalMissing)
	}
	if s.MaxAmount != "" {
		max, _ := new(big.Rat).SetString(s.MaxAmount)
		if amt.Cmp(max) > 0 {
			return ErrLimitExceeded
		}
	}
	return nil
}

// BuildRequest turns the canonical subject into the request body, then PROVES the body carries exactly the
// canonical amount by reading it back out. A profile or a bug that mangles the amount is refused before
// anything is sent (INT-NP: amount mutation / transformed total differs).
func (s Spec) BuildRequest(c Canonical) ([]byte, error) { return s.BuildRequestDoc(c, nil) }

// BuildRequestDoc is BuildRequest for a profile that embeds the approved document. The document is validated
// against the authority schema first; a document that fails is never sent.
func (s Spec) BuildRequestDoc(c Canonical, doc []byte) ([]byte, error) {
	if err := s.CheckAgainstProfile(c); err != nil {
		return nil, err
	}
	if len(s.Schema) > 0 {
		sch, err := CompileSchema(s.Schema)
		if err != nil {
			return nil, err
		}
		if err := sch.ValidateDocument(doc); err != nil {
			return nil, err
		}
	}
	var amt *big.Rat
	if c["amount"] != "" {
		amt, _ = new(big.Rat).SetString(c["amount"])
	}
	body := map[string]any{}
	for _, f := range s.Submit.Body {
		switch f.From {
		case "const":
			setPath(body, f.Name, f.Const)
		case "document":
			if len(doc) == 0 {
				return nil, ErrDocumentMissing
			}
			var d any
			dec := json.NewDecoder(bytes.NewReader(doc))
			dec.UseNumber()
			if err := dec.Decode(&d); err != nil {
				return nil, ErrDocumentMissing
			}
			setPath(body, f.Name, d)
		case "canonical":
			if f.Source == "amount" {
				setPath(body, f.Name, s.renderAmount(amt, c["currency"]))
				continue
			}
			v, ok := c[f.Source]
			if !ok {
				return nil, fmt.Errorf("%w: %s", ErrCanonicalMissing, f.Source)
			}
			setPath(body, f.Name, v)
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if amt != nil && s.Submit.AmountField != "" {
		if err := s.verifyAmount(raw, amt, c["currency"]); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func (s Spec) renderAmount(amt *big.Rat, currency string) any {
	switch s.Submit.AmountFormat {
	case "minor":
		exp := minorExponent(currency)
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
		n := new(big.Rat).Mul(amt, new(big.Rat).SetInt(scale))
		return json.Number(new(big.Int).Quo(n.Num(), n.Denom()).String())
	case "number":
		return json.Number(canonicalDecimal(amt))
	}
	return canonicalDecimal(amt)
}

func (s Spec) verifyAmount(raw []byte, want *big.Rat, currency string) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var back map[string]any
	if err := dec.Decode(&back); err != nil {
		return ErrAmountMismatch
	}
	v, ok := getPath(back, s.Submit.AmountField)
	if !ok {
		return ErrAmountMismatch
	}
	got, ok := new(big.Rat).SetString(asString(v))
	if !ok {
		return ErrAmountMismatch
	}
	if s.Submit.AmountFormat == "minor" {
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(minorExponent(currency))), nil)
		got.Quo(got, new(big.Rat).SetInt(scale))
	}
	if got.Cmp(want) != 0 {
		return ErrAmountMismatch
	}
	return nil
}

// canonicalDecimal renders a rational with no trailing zeros past two places ("10.50" stays "10.50",
// "10" stays "10.00").
func canonicalDecimal(r *big.Rat) string {
	s := r.FloatString(6)
	if i := strings.Index(s, "."); i >= 0 {
		frac := strings.TrimRight(s[i+1:], "0")
		for len(frac) < 2 {
			frac += "0"
		}
		s = s[:i+1] + frac
	}
	return s
}

// Hash is SHA-256 over the canonical profile spec.
func (s Spec) Hash() string { return hashJSON(s.Normalize()) }

// Now is a clock seam for callers that need one.
var Now = func() time.Time { return time.Now().UTC() }

// Validate checks a callback definition is complete and safe to use.
func (c Callback) Validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrSpecInvalid, fmt.Sprintf(f, a...)) }
	if c.Scheme != "hmac-sha256-hex" && c.Scheme != "zoiko-v1" {
		return bad("callback.scheme must be hmac-sha256-hex or zoiko-v1")
	}
	if c.Header != "" && !headerRe.MatchString(c.Header) {
		return bad("callback.header is malformed")
	}
	if c.Scheme == "zoiko-v1" && (c.ToleranceSeconds < 30 || c.ToleranceSeconds > 3600) {
		return bad("a zoiko-v1 callback needs tolerance_seconds between 30 and 3600")
	}
	for _, p := range []string{c.EventRefPath, c.PaymentRefPath, c.StatusPath} {
		if !pathKeyRe.MatchString(p) {
			return bad("a callback path is missing or malformed")
		}
	}
	if c.EventTimePath != "" && !pathKeyRe.MatchString(c.EventTimePath) {
		return bad("callback.event_time_path is malformed")
	}
	if len(c.StatusMap) == 0 {
		return bad("callback.status_map is required")
	}
	for _, v := range c.StatusMap {
		if v != StatusAcknowledged && v != StatusRejected && v != StatusPending && v != "SETTLED" && v != StatusAccepted {
			return bad("a callback status maps to ACKNOWLEDGED, PENDING, REJECTED or SETTLED")
		}
	}
	return nil
}
