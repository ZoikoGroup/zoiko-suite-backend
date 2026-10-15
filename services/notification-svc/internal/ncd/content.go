package ncd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── hashing and endpoint helpers ────────────────────────────────────────────

// SHA256Hex is the single hash every content, endpoint and variable hash uses.
func SHA256Hex(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0x1f})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NormalizeEndpoint canonicalizes an address so the same mailbox always
// hashes the same way, whichever provider or caller spelled it.
func NormalizeEndpoint(channel, address string) string {
	a := strings.TrimSpace(address)
	if channel == ChannelEmail {
		a = strings.ToLower(a)
	}
	return a
}

// EndpointHash is the key suppressions and plans match on (§7.3: the list
// holds hashes, not addresses).
func EndpointHash(channel, address string) string {
	return SHA256Hex("endpoint", channelFamily(channel), NormalizeEndpoint(channel, address))
}

// channelFamily makes an email address hash identically whichever channel
// label reached it; a principal's inbox is keyed by principal.
func channelFamily(channel string) string { return channel }

// MaskEndpoint renders an endpoint for display without disclosing it.
func MaskEndpoint(channel, address string) string {
	a := NormalizeEndpoint(channel, address)
	if channel == ChannelEmail {
		at := strings.LastIndex(a, "@")
		if at <= 0 {
			return "***"
		}
		return a[:1] + "***" + a[at:]
	}
	if len(a) <= 4 {
		return "***"
	}
	return "***" + a[len(a)-4:]
}

// ── template scanning ───────────────────────────────────────────────────────

var placeholderRE = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// Placeholders returns the distinct variable names a text references, sorted.
func Placeholders(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range placeholderRE.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// SchemaHash identifies the variable surface a template version depends on.
func SchemaHash(subject, body string) string {
	names := append(Placeholders(subject), Placeholders(body)...)
	sort.Strings(names)
	return SHA256Hex(append([]string{"schema"}, names...)...)
}

// ContentHash identifies a template version's exact bytes.
func ContentHash(channel, locale, subject, body string) string {
	return SHA256Hex("template", channel, locale, subject, body)
}

var localeRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)

func ValidLocale(l string) bool { return localeRE.MatchString(l) }

// ── validation (POST /v1/templates/{id}/validate, §4.4) ─────────────────────

// Finding is one validation result. BLOCK findings keep a template out of
// REVIEW; WARN findings are recorded and shown to the approver.
type Finding struct {
	Check    string     `json:"check"`
	Severity string     `json:"severity"` // BLOCK or WARN
	Code     ReasonCode `json:"reason_code,omitempty"`
	Detail   string     `json:"detail"`
}

// ValidationReport is what validate returns and what approve relies on.
type ValidationReport struct {
	Passed    bool      `json:"passed"`
	Findings  []Finding `json:"findings"`
	CheckedAt time.Time `json:"checked_at"`
	// Checks lists every rule that ran, passing or not, so an approver can
	// see what was — and was not — examined.
	Checks []string `json:"checks"`
}

var (
	scriptRE         = regexp.MustCompile(`(?i)<\s*script`)
	eventAttrRE      = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	jsURLRE          = regexp.MustCompile(`(?i)(href|src)\s*=\s*["']?\s*javascript:`)
	embedRE          = regexp.MustCompile(`(?i)<\s*(iframe|object|embed|form|base|meta)\b`)
	imgTagRE         = regexp.MustCompile(`(?i)<\s*img\b[^>]*>`)
	altRE            = regexp.MustCompile(`(?i)\salt\s*=\s*["'][^"']*["']`)
	srcRE            = regexp.MustCompile(`(?i)\ssrc\s*=\s*["']([^"']*)["']`)
	pixelRE          = regexp.MustCompile(`(?i)(width|height)\s*=\s*["']?\s*[01]\s*["']?`)
	hrefRE           = regexp.MustCompile(`(?i)\shref\s*=\s*["']([^"']*)["']`)
	promotionalRE    = regexp.MustCompile(`(?i)(data-ncd-block\s*=\s*["']promotional["']|<!--\s*ncd:promotional\s*-->)`)
	subjectSensitive = []string{
		"salary", "wage", "bonus amount", "bank account", "iban", "sort code", "routing number",
		"tax id", "ssn", "social security", "national insurance", "diagnosis", "medical", "health condition",
		"disciplinary", "misconduct", "dismissal", "termination reason", "grievance", "pregnan", "disability",
	}
)

// ValidateTemplate runs schema, content, security, accessibility and policy
// validation against the intent the template belongs to (§4.5 validate).
func ValidateTemplate(intent Intent, tv TemplateVersion, now time.Time) ValidationReport {
	r := ValidationReport{CheckedAt: now}
	add := func(check, sev string, code ReasonCode, format string, a ...any) {
		r.Findings = append(r.Findings, Finding{Check: check, Severity: sev, Code: code, Detail: fmt.Sprintf(format, a...)})
	}
	ran := func(check string) { r.Checks = append(r.Checks, check) }

	ran("channel_allowed_by_intent")
	if !intent.Allows(tv.Channel) {
		add("channel_allowed_by_intent", "BLOCK", NCD011NoCompliantChannel,
			"channel %s is not in the intent's allowed channels %v", tv.Channel, intent.AllowedChannels)
	}

	ran("locale_format")
	if !ValidLocale(tv.Locale) {
		add("locale_format", "BLOCK", NCD005LocaleNotApproved, "locale %q is not a recognised locale tag", tv.Locale)
	}
	for _, l := range tv.CompatibleLocales {
		if !ValidLocale(l) || l == tv.Locale {
			add("locale_format", "BLOCK", NCD005LocaleNotApproved, "compatible locale %q is malformed or equals the template's own locale", l)
		}
	}

	ran("body_present")
	if strings.TrimSpace(tv.Body) == "" {
		add("body_present", "BLOCK", NCD004TemplateVariableInvalid, "body is empty")
	}
	ran("subject_present")
	if tv.Channel == ChannelEmail && strings.TrimSpace(tv.Subject) == "" {
		add("subject_present", "BLOCK", NCD004TemplateVariableInvalid, "an EMAIL template needs a subject")
	}

	// Schema: every placeholder must be in the typed contract (NP-07 at
	// publication time; Render enforces it again for values).
	ran("variables_in_contract")
	for _, name := range append(Placeholders(tv.Subject), Placeholders(tv.Body)...) {
		if _, ok := intent.Variable(name); !ok {
			add("variables_in_contract", "BLOCK", NCD004TemplateVariableInvalid,
				"placeholder {{%s}} is not declared in the intent's variable contract", name)
		}
	}

	// Header injection: a literal CR/LF in the subject template.
	ran("subject_header_safe")
	if strings.ContainsAny(tv.Subject, "\r\n") {
		add("subject_header_safe", "BLOCK", NCD004TemplateVariableInvalid, "subject contains a line break (header injection)")
	}

	// INV-17 / NP-32: no S2/S3 facts on low-confidentiality surfaces.
	ran("subject_sensitivity")
	for _, name := range Placeholders(tv.Subject) {
		if v, ok := intent.Variable(name); ok && v.Sensitivity.Rank() >= 2 {
			add("subject_sensitivity", "BLOCK", NCD004TemplateVariableInvalid,
				"subject references {{%s}}, classified %s; S2/S3 values are prohibited in subjects", name, v.Sensitivity)
		}
	}
	if intent.Sensitivity.Rank() >= 2 || intent.PurposeClass == PurposeRegulated {
		lower := strings.ToLower(tv.Subject)
		for _, term := range subjectSensitive {
			if strings.Contains(lower, term) {
				add("subject_sensitivity", "BLOCK", NCD004TemplateVariableInvalid,
					"subject names a sensitive fact (%q); keep it on the authenticated surface", term)
			}
		}
	}

	// §11.2: an S2/S3 payload belongs behind authentication. Email, SMS and
	// push carry minimal notice text; only the in-app surface may render the
	// sensitive values themselves (NP-31).
	ran("channel_sensitivity")
	if tv.Channel != ChannelInApp {
		for _, name := range Placeholders(tv.Body) {
			if v, ok := intent.Variable(name); ok && v.Sensitivity.Rank() >= 2 {
				add("channel_sensitivity", "BLOCK", NCD004TemplateVariableInvalid,
					"%s body references {{%s}} (%s); S2/S3 values may only render on the authenticated in-app surface — send a minimal notice with a secure link",
					tv.Channel, name, v.Sensitivity)
			}
		}
	}

	if tv.Channel == ChannelEmail || tv.Channel == ChannelInApp {
		ran("html_script")
		if scriptRE.MatchString(tv.Body) || jsURLRE.MatchString(tv.Body) {
			add("html_script", "BLOCK", NCD004TemplateVariableInvalid, "body contains a script or javascript: URL")
		}
		ran("html_event_attributes")
		if eventAttrRE.MatchString(tv.Body) {
			add("html_event_attributes", "BLOCK", NCD004TemplateVariableInvalid, "body contains an inline event handler attribute")
		}
		ran("html_embedded_content")
		if embedRE.MatchString(tv.Body) {
			add("html_embedded_content", "BLOCK", NCD004TemplateVariableInvalid, "body contains an iframe/object/embed/form/base/meta element")
		}

		protected := intent.Sensitivity.Rank() >= 2 || intent.PurposeClass == PurposeRegulated || intent.PurposeClass == PurposeSecurityCritical
		ran("external_assets_and_tracking")
		ran("accessibility_alt_text")
		for _, tag := range imgTagRE.FindAllString(tv.Body, -1) {
			if !altRE.MatchString(tag) {
				add("accessibility_alt_text", "BLOCK", "", "an <img> has no alt text: %s", truncate(tag, 80))
			}
			src := ""
			if m := srcRE.FindStringSubmatch(tag); m != nil {
				src = m[1]
			}
			external := strings.HasPrefix(strings.ToLower(src), "http")
			if external && pixelRE.MatchString(tag) && !intent.ClassifiedAsMarketing() {
				add("external_assets_and_tracking", "BLOCK", "", "a 0/1-pixel external image is a tracking pixel; tracking needs a permitted marketing purpose (§4.4)")
			} else if external && protected {
				add("external_assets_and_tracking", "BLOCK", "", "external image %q in a sensitive/regulated/security template; use controlled local assets (NP-54)", src)
			} else if external {
				add("external_assets_and_tracking", "WARN", "", "external image %q; it renders only while the host is reachable", src)
			}
		}

		ran("url_allowlist")
		for _, m := range hrefRE.FindAllStringSubmatch(tv.Body, -1) {
			href := strings.TrimSpace(m[1])
			if href == "" || strings.HasPrefix(href, "{{") || strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "#") {
				continue
			}
			if err := checkURL(href, intent.ApprovedURLDomains); err != nil {
				add("url_allowlist", "BLOCK", NCD004TemplateVariableInvalid, "link %q: %v", truncate(href, 80), err)
			}
		}
	}

	// NP-48 / §11.4: promotional content in a non-marketing intent.
	ran("promotional_content")
	if promotionalRE.MatchString(tv.Body) && !intent.ClassifiedAsMarketing() {
		add("promotional_content", "BLOCK", NCD009MarketingPermissionBlock,
			"a promotional block in a %s template; the whole message would have to pass marketing permission (§11.4)", intent.PurposeClass)
	}

	r.Passed = true
	for _, f := range r.Findings {
		if f.Severity == "BLOCK" {
			r.Passed = false
		}
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// checkURL enforces §4.4 URL manipulation: https only, approved domain only.
func checkURL(raw string, approved []string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("not an absolute URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme %q; only https links are permitted", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range approved {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
			return nil
		}
	}
	return fmt.Errorf("host %q is not an approved domain for this intent", host)
}

// ── rendering (§4.4, INV-05) ────────────────────────────────────────────────

// Rendered is one channel's rendered content and its lineage hashes.
type Rendered struct {
	Subject        string
	Body           string
	SubjectHash    string
	BodyHash       string
	ContentHash    string
	VariableHashes map[string]string
}

var (
	dateRE  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	moneyRE = regexp.MustCompile(`^-?\d{1,15}(\.\d{1,4})? [A-Z]{3}$`)
)

// ValidateVariables checks supplied values against the contract without
// rendering: unknown names are refused (NP-07), required names must be
// present (NP-06), every value must match its declared type, and no value may
// carry a line break into a header (NP-08).
func ValidateVariables(intent Intent, vars map[string]string) *Refusal {
	for name := range vars {
		if _, ok := intent.Variable(name); !ok {
			return Refuse(NCD004TemplateVariableInvalid, fmt.Sprintf("variable %q is not in the intent's contract", name))
		}
	}
	for _, spec := range intent.VariableContract {
		val, present := vars[spec.Name]
		if !present || val == "" {
			if spec.Required {
				return Refuse(NCD004TemplateVariableInvalid, fmt.Sprintf("required variable %q is missing", spec.Name))
			}
			continue
		}
		if err := checkValue(spec, val, intent.ApprovedURLDomains); err != nil {
			return Refuse(NCD004TemplateVariableInvalid, fmt.Sprintf("variable %q: %v", spec.Name, err))
		}
	}
	return nil
}

func checkValue(spec VariableSpec, val string, approvedDomains []string) error {
	if strings.ContainsAny(val, "\r\n") && spec.Type != "text" {
		return fmt.Errorf("contains a line break (header injection refused)")
	}
	if spec.MaxLength > 0 && len(val) > spec.MaxLength {
		return fmt.Errorf("longer than %d characters", spec.MaxLength)
	}
	switch spec.Type {
	case "", "string", "text":
	case "number":
		if _, err := strconv.ParseFloat(val, 64); err != nil {
			return fmt.Errorf("not a number")
		}
	case "date":
		if !dateRE.MatchString(val) {
			return fmt.Errorf("not a YYYY-MM-DD date")
		}
		if _, err := time.Parse("2006-01-02", val); err != nil {
			return fmt.Errorf("not a valid date")
		}
	case "money":
		if !moneyRE.MatchString(val) {
			return fmt.Errorf("not an amount with an ISO currency, e.g. \"120.50 EUR\"")
		}
	case "email":
		if _, err := mail.ParseAddress(val); err != nil {
			return fmt.Errorf("not an email address")
		}
	case "url":
		if err := checkURL(val, approvedDomains); err != nil {
			return err
		}
	case "enum":
		ok := false
		for _, a := range spec.AllowedValues {
			if a == val {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("not one of %v", spec.AllowedValues)
		}
	default:
		return fmt.Errorf("the contract declares unknown type %q", spec.Type)
	}
	return nil
}

// ValidVariableType reports whether a contract type is supported.
func ValidVariableType(t string) bool {
	switch t {
	case "", "string", "text", "number", "date", "money", "email", "url", "enum":
		return true
	}
	return false
}

// Render renders one template version against the intent's contract.
//
// A placeholder whose value is missing renders its schema-defined fallback
// text or blocks — it never renders blank, "None" or a stale value (§4.4,
// INV-05). HTML channels escape every value; a subject is plain text and
// refuses line breaks.
func Render(intent Intent, tv TemplateVersion, vars map[string]string) (Rendered, *Refusal) {
	if ref := ValidateVariables(intent, vars); ref != nil {
		return Rendered{}, ref
	}
	var refusal *Refusal
	sub := func(text string, htmlContext bool) string {
		return placeholderRE.ReplaceAllStringFunc(text, func(m string) string {
			name := placeholderRE.FindStringSubmatch(m)[1]
			spec, ok := intent.Variable(name)
			if !ok {
				refusal = Refuse(NCD004TemplateVariableInvalid, fmt.Sprintf("template references undeclared variable %q", name))
				return ""
			}
			val, present := vars[name]
			if !present || val == "" {
				if spec.FallbackText == "" {
					refusal = Refuse(NCD004TemplateVariableInvalid,
						fmt.Sprintf("variable %q is referenced, has no value and no governed fallback text", name))
					return ""
				}
				val = spec.FallbackText
			}
			if htmlContext {
				return html.EscapeString(val)
			}
			return val
		})
	}
	htmlBody := tv.Channel == ChannelEmail || tv.Channel == ChannelInApp
	subject := sub(tv.Subject, false)
	body := sub(tv.Body, htmlBody)
	if refusal != nil {
		return Rendered{}, refusal
	}
	if strings.ContainsAny(subject, "\r\n") {
		return Rendered{}, Refuse(NCD004TemplateVariableInvalid, "rendered subject contains a line break (header injection refused)")
	}

	hashes := make(map[string]string, len(vars))
	for k, v := range vars {
		hashes[k] = SHA256Hex("variable", k, v)
	}
	out := Rendered{
		Subject:        subject,
		Body:           body,
		SubjectHash:    SHA256Hex("subject", subject),
		BodyHash:       SHA256Hex("body", body),
		VariableHashes: hashes,
	}
	out.ContentHash = SHA256Hex("rendered", tv.Channel, out.SubjectHash, out.BodyHash)
	return out, nil
}

// ContentHashWithAttachments folds the pinned attachment manifest into a
// rendered content hash, so an attempt's payload hash covers the exact DRC
// versions it carried (§6.1 content_hash, NP-34).
func ContentHashWithAttachments(renderHash string, atts []Attachment) string {
	raw, _ := json.Marshal(atts)
	return SHA256Hex("payload", renderHash, string(raw))
}

// SyntheticVariables fills a contract with type-correct synthetic values for
// POST /v1/render-previews (§4.5: "synthetic/redacted data only").
func SyntheticVariables(intent Intent) map[string]string {
	out := map[string]string{}
	domain := "example.com"
	if len(intent.ApprovedURLDomains) > 0 {
		domain = intent.ApprovedURLDomains[0]
	}
	for _, v := range intent.VariableContract {
		switch v.Type {
		case "number":
			out[v.Name] = "42"
		case "date":
			out[v.Name] = "2026-01-31"
		case "money":
			out[v.Name] = "100.00 USD"
		case "email":
			out[v.Name] = "recipient@example.com"
		case "url":
			out[v.Name] = "https://" + domain + "/preview"
		case "enum":
			if len(v.AllowedValues) > 0 {
				out[v.Name] = v.AllowedValues[0]
			}
		default:
			out[v.Name] = "[" + strings.ToUpper(v.Name) + "]"
		}
	}
	return out
}

// RedactVariables replaces every S2/S3 value supplied to a preview, so a
// preview can never become a way to read sensitive content back out.
func RedactVariables(intent Intent, vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		spec, ok := intent.Variable(k)
		if ok && spec.Sensitivity.Rank() >= 2 {
			if spec.Type == "url" || spec.Type == "email" || spec.Type == "date" || spec.Type == "money" || spec.Type == "number" || spec.Type == "enum" {
				out[k] = SyntheticVariables(Intent{VariableContract: []VariableSpec{spec}, ApprovedURLDomains: intent.ApprovedURLDomains})[k]
			} else {
				out[k] = "[REDACTED:" + string(spec.Sensitivity) + "]"
			}
			continue
		}
		out[k] = v
	}
	return out
}
