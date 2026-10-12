package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ── immutable source representation ─────────────────────────────────────────

// SourcePayload is the canonical form of what the supplier submitted. Its
// sha256 is the invoice's source hash. Field order is fixed by the struct, maps
// are marshalled with sorted keys, so the same submission always hashes the
// same. Server-assigned fields (ids, tenant, correlation, timestamps) are
// deliberately excluded: the hash identifies the SUPPLIER's document content.
type SourcePayload struct {
	DocumentType   string                         `json:"document_type"`
	LegalEntityID  string                         `json:"legal_entity_id"`
	VendorID       string                         `json:"vendor_id"`
	InvoiceNumber  string                         `json:"invoice_number"`
	Amount         float64                        `json:"amount"`
	CurrencyCode   string                         `json:"currency_code"`
	InvoiceDate    string                         `json:"invoice_date"`
	SupplyDate     string                         `json:"supply_date"`
	DueDate        string                         `json:"due_date"`
	PurchaseOrder  string                         `json:"purchase_order_id,omitempty"`
	GoodsReceipt   string                         `json:"goods_receipt_ref,omitempty"`
	DocumentID     string                         `json:"invoice_document_id,omitempty"`
	AttachmentHash string                         `json:"attachment_hash,omitempty"`
	Withholding    string                         `json:"withholding_determination_id,omitempty"`
	BankDetails    *BankDetails                   `json:"extracted_bank_details,omitempty"`
	Lines          []CreateVendorInvoiceLineInput `json:"lines"`
}

// Canonical returns the canonical JSON bytes and their sha256 hex digest.
func (p SourcePayload) Canonical() ([]byte, string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, "", fmt.Errorf("canonical source payload: %w", err)
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

var hexSHA256 = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// ValidSHA256 reports whether s is a 64-char hex digest.
func ValidSHA256(s string) bool { return hexSHA256.MatchString(s) }

// ── invoice-supplied bank details: evidence only ────────────────────────────

type BankDetails struct {
	AccountName          string `json:"account_name,omitempty"`
	AccountIdentifier    string `json:"account_identifier"` // IBAN / account number as printed
	FinancialInstitution string `json:"financial_institution,omitempty"`
	CountryCode          string `json:"country_code,omitempty"`
	Currency             string `json:"currency,omitempty"`
}

// Value/Scan are provided through JSON helpers in the store (jsonb).

// NormalizeAccount upper-cases and strips everything but letters and digits, so
// "gb29 nwbk-6016" and "GB29NWBK6016" compare equal.
func NormalizeAccount(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// PayeeCheck records how the invoice bank evidence compared with ORG-10.
type PayeeCheck struct {
	Basis            string    `json:"basis"` // FULL_IDENTIFIER | LAST4 | NO_ACTIVE_DESTINATION
	Outcome          string    `json:"outcome"`
	ORG10Destination string    `json:"org10_destination_id,omitempty"`
	CheckedAt        time.Time `json:"checked_at"`
	ResolvedBy       string    `json:"resolved_by,omitempty"`
	ResolvedReason   string    `json:"resolved_reason,omitempty"`
}

// ComparePayee decides whether the invoice-supplied account equals the ORG-10
// active destination. identifier may be masked (ORG-10 masks it for callers
// without privilege); in that case only the last 4 characters can be compared
// and a mismatch OR an inability to compare is treated as a mismatch -- a
// comparison that cannot be made never counts as a match.
func ComparePayee(invoice BankDetails, orgIdentifier, orgLast4 string) (match bool, basis string) {
	inv := NormalizeAccount(invoice.AccountIdentifier)
	if inv == "" {
		return false, "FULL_IDENTIFIER"
	}
	if orgIdentifier != "" && !strings.ContainsAny(orgIdentifier, "*•") {
		return NormalizeAccount(orgIdentifier) == inv, "FULL_IDENTIFIER"
	}
	last4 := NormalizeAccount(orgLast4)
	if last4 == "" {
		last4 = NormalizeAccount(orgIdentifier)
		if len(last4) > 4 {
			last4 = last4[len(last4)-4:]
		}
	}
	if len(last4) < 4 || len(inv) < 4 {
		return false, "LAST4"
	}
	return inv[len(inv)-4:] == last4[len(last4)-4:], "LAST4"
}

// ── TAX / withholding provenance ────────────────────────────────────────────

// TaxProvenance is one verified reference into tax-determination-svc. The
// numbers are TAX's, never AP's.
type TaxProvenance struct {
	DeterminationID    string    `json:"determination_id"`
	Status             string    `json:"status"`
	RuleID             string    `json:"rule_id,omitempty"`
	TaxLogicSnapshotID string    `json:"tax_logic_snapshot_id,omitempty"`
	CalculatedTax      float64   `json:"calculated_tax"`
	Currency           string    `json:"currency"`
	LineNumbers        []int     `json:"line_numbers,omitempty"`
	VerifiedAt         time.Time `json:"verified_at"`
}

// ResultFingerprint is the version of the TAX result: rule pack identity, the
// content snapshot and the amount. If any of it differs from what was verified,
// the result has changed.
func (p TaxProvenance) ResultFingerprint() string {
	return fmt.Sprintf("%s|%s|%s|%s|%.2f|%s", p.DeterminationID, p.Status, p.RuleID, p.TaxLogicSnapshotID, p.CalculatedTax, p.Currency)
}

// TaxResultHash folds every provenance (tax and withholding) into one digest in a
// deterministic order.
func TaxResultHash(tax []TaxProvenance, withholding *TaxProvenance) string {
	parts := make([]string, 0, len(tax)+1)
	for _, p := range tax {
		parts = append(parts, "T:"+p.ResultFingerprint())
	}
	if withholding != nil {
		parts = append(parts, "W:"+withholding.ResultFingerprint())
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// TaxAmountsAgree compares the invoice-stated tax with the determination's, in
// minor units.
func TaxAmountsAgree(stated, determined float64) bool {
	return math.Abs(stated-determined) <= 0.01+1e-9
}

// ── duplicate detection ─────────────────────────────────────────────────────

// NormalizeInvoiceNumber reduces a supplier invoice number to its comparison
// form: lower-cased, leading zeros of every digit run removed (keeping at least
// one digit), then everything that is not a letter or digit dropped. "INV-0012",
// "inv 12", "Inv/12." and "INV0012" all normalise to "inv12". This is what
// defeats the formatting-altered near-duplicate bypass (negative path #14).
func NormalizeInvoiceNumber(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); {
		r := runes[i]
		if r >= '0' && r <= '9' {
			j := i
			for j < len(runes) && runes[j] >= '0' && runes[j] <= '9' {
				j++
			}
			k := i
			for k < j-1 && runes[k] == '0' {
				k++
			}
			b.WriteString(string(runes[k:j]))
			i = j
			continue
		}
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
		i++
	}
	return b.String()
}

// DuplicateConfig is the configurable part of near-duplicate detection. It is
// persisted with every assessment so the assessment is reproducible.
type DuplicateConfig struct {
	DateWindowDays int     `json:"date_window_days"` // |invoice_date delta| for the amount rule
	NearThreshold  float64 `json:"near_threshold"`   // score at or above which an invoice is quarantined
	AmountToleranceCents int64 `json:"amount_tolerance_cents"`
}

func DefaultDuplicateConfig() DuplicateConfig {
	return DuplicateConfig{DateWindowDays: 7, NearThreshold: 0.75, AmountToleranceCents: 0}
}

// DuplicateCandidate is the slice of an existing invoice the assessment needs.
type DuplicateCandidate struct {
	InvoiceID         string       `json:"invoice_id"`
	InvoiceNumber     string       `json:"invoice_number"`
	NumberNormalized  string       `json:"number_normalized"`
	Amount            float64      `json:"amount"`
	CurrencyCode      string       `json:"currency_code"`
	InvoiceDate       CalendarDate `json:"invoice_date"`
	IntakeState       IntakeState  `json:"intake_state"`
	DocumentType      string       `json:"document_type"`
}

// DuplicateMatch is one candidate that scored.
type DuplicateMatch struct {
	InvoiceID string   `json:"invoice_id"`
	Score     float64  `json:"score"`
	Reasons   []string `json:"reasons"`
}

// DuplicateAssessment is the persisted, reproducible result.
type DuplicateAssessment struct {
	AssessmentID      string                 `json:"assessment_id"`
	TenantID          string                 `json:"tenant_id"`
	InvoiceID         string                 `json:"invoice_id"`
	Verdict           string                 `json:"verdict"` // CLEAR | NEAR_DUPLICATE
	Score             float64                `json:"score"`
	ExactKey          string                 `json:"exact_key"`
	NearKey           string                 `json:"near_key"`
	Inputs            map[string]any         `json:"inputs"`
	Config            DuplicateConfig        `json:"config"`
	MatchedInvoiceIDs []string               `json:"matched_invoice_ids"`
	Matches           []DuplicateMatch       `json:"matches"`
	TriggerCommand    string                 `json:"trigger_command"`
	AssessedAt        time.Time              `json:"assessed_at"`
}

const (
	VerdictClear         = "CLEAR"
	VerdictNearDuplicate = "NEAR_DUPLICATE"
)

// AssessDuplicates is a PURE function of (invoice facts, candidates, config): the
// same inputs always give the same assessment, which is what lets the persisted
// inputs reproduce it. The invoice itself must not appear among the candidates.
//
// Scoring (same supplier, same tenant, same document class only):
//   - normalised number equal                          -> 1.00
//   - amount + currency equal and invoice dates within
//     the window                                       -> 0.60 + 0.30 x number similarity
//
// A candidate scoring at or above NearThreshold makes the verdict NEAR_DUPLICATE.
func AssessDuplicates(tenantID, invoiceID, vendorID, number, docType string, amount float64, currency string, invoiceDate time.Time, cands []DuplicateCandidate, cfg DuplicateConfig) DuplicateAssessment {
	norm := NormalizeInvoiceNumber(number)
	a := DuplicateAssessment{
		TenantID: tenantID, InvoiceID: invoiceID, Verdict: VerdictClear,
		ExactKey: fmt.Sprintf("%s|%s|%s", tenantID, vendorID, number),
		NearKey:  fmt.Sprintf("%s|%s|%s", tenantID, vendorID, norm),
		Config:   cfg, MatchedInvoiceIDs: []string{}, Matches: []DuplicateMatch{},
		Inputs: map[string]any{
			"vendor_id": vendorID, "invoice_number": number, "number_normalized": norm,
			"document_type": docType, "amount": amount, "currency_code": currency,
			"invoice_date": invoiceDate.UTC().Format("2006-01-02"),
			"candidates":   cands,
		},
	}
	sorted := append([]DuplicateCandidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].InvoiceID < sorted[j].InvoiceID })
	for _, c := range sorted {
		if c.InvoiceID == invoiceID || c.DocumentType != docType || c.IntakeState == IntakeRejected {
			continue
		}
		var score float64
		var reasons []string
		if c.NumberNormalized == norm && norm != "" {
			score = 1.0
			if c.InvoiceNumber == number {
				reasons = append(reasons, "EXACT_NUMBER")
			} else {
				reasons = append(reasons, "NORMALIZED_NUMBER_EQUAL")
			}
		}
		days := math.Abs(invoiceDate.Sub(c.InvoiceDate.Time).Hours() / 24)
		if c.CurrencyCode == currency && cents(c.Amount)-cents(amount) <= cfg.AmountToleranceCents &&
			cents(amount)-cents(c.Amount) <= cfg.AmountToleranceCents && int(days) <= cfg.DateWindowDays {
			s := 0.60 + 0.30*similarity(norm, c.NumberNormalized)
			reasons = append(reasons, "SAME_AMOUNT_CURRENCY_DATE_WINDOW")
			if s > score {
				score = s
			}
		}
		score = math.Round(score*10000) / 10000
		if score > 0 && score >= cfg.NearThreshold {
			a.Matches = append(a.Matches, DuplicateMatch{InvoiceID: c.InvoiceID, Score: score, Reasons: reasons})
			a.MatchedInvoiceIDs = append(a.MatchedInvoiceIDs, c.InvoiceID)
			if score > a.Score {
				a.Score = score
			}
		}
	}
	if len(a.Matches) > 0 {
		a.Verdict = VerdictNearDuplicate
	}
	return a
}

// similarity is 1 - levenshtein/maxLen over the normalised numbers.
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	n := len(ra)
	if len(rb) > n {
		n = len(rb)
	}
	if n == 0 {
		return 1
	}
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return 1 - float64(prev[len(rb)])/float64(n)
}

// ── accounting event (local mirror of services/_contract/accounting) ─────────

// AccountingEventPayload mirrors services/_contract/accounting.AccountingEvent
// field-for-field (same JSON names). It is declared locally because this
// service builds from its own directory (Docker context) and cannot import the
// shared contract module; keep it in step with that type. AP-05 never posts to
// the ledger: it emits this fact and the posting engine consumes it.
type AccountingEventPayload struct {
	AccountingEventID    string          `json:"accounting_event_id"`
	TenantID             string          `json:"tenant_id"`
	LegalEntityID        string          `json:"legal_entity_id"`
	SourceDomain         string          `json:"source_domain"`
	SourceObjectTable    string          `json:"source_object_table"`
	SourceObjectID       string          `json:"source_object_id"`
	EventType            string          `json:"event_type"`
	OccurredAt           time.Time       `json:"occurred_at"`
	EffectiveDate        time.Time       `json:"effective_date"`
	AmountBasis          json.RawMessage `json:"amount_basis"`
	PostingPolicyVersion string          `json:"posting_policy_version"`
	Status               string          `json:"status"`
	CreatedAt            time.Time       `json:"created_at"`
}
