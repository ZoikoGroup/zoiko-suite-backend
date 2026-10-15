// Package envelope defines the canonical request and transition contracts for ZoikoSuite.
//
// This file implements the Canonical Subject Fingerprint Engine per ZS-STATE-001
// §6.1 ("Approval subject fingerprint").
//
// Doctrine (ZS-STATE-001 §6.1):
// "For every approval type, the owning service defines the material fields whose
// change invalidates approval. A canonical hash/fingerprint is calculated over
// normalized material facts and stored on the approval request. Examples:
// invoice total, currency, supplier bank account, payment destination, journal
// lines, tax return totals, contract version, legal entity and effective date."
package envelope

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// FingerprintPrefix is the platform standard prefix for SHA-256 subject fingerprints.
const FingerprintPrefix = "sha256:"

// IsValidFingerprint reports whether s is a well-formed SHA-256 subject fingerprint
// in the format "sha256:<64 lowercase hex characters>".
func IsValidFingerprint(s string) bool {
	if !strings.HasPrefix(s, FingerprintPrefix) {
		return false
	}
	hexPart := strings.TrimPrefix(s, FingerprintPrefix)
	if len(hexPart) != 64 {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// FingerprintBuilder constructs a deterministic subject fingerprint over a set of
// material domain attributes.
//
// Canonicalization rules:
// 1. Keys and values are trimmed of leading and trailing whitespace.
// 2. Keys are sorted lexicographically before hashing to ensure order-independence.
// 3. Monetary amounts are normalized with currency and exact 2-decimal precision (or 4-decimal where specified).
// 4. Collection items (such as line item IDs or line hashes) are sorted lexicographically.
// 5. Output is strictly prefixed with "sha256:" followed by 64 lowercase hexadecimal characters.
type FingerprintBuilder struct {
	fields      map[string]string
	collections map[string][]string
}

// NewFingerprintBuilder allocates an empty FingerprintBuilder.
func NewFingerprintBuilder() *FingerprintBuilder {
	return &FingerprintBuilder{
		fields:      make(map[string]string),
		collections: make(map[string][]string),
	}
}

// Set sets a scalar material field on the builder. Empty keys are ignored.
// Key and value are trimmed of whitespace.
func (b *FingerprintBuilder) Set(key, value string) *FingerprintBuilder {
	k := strings.TrimSpace(key)
	if k == "" {
		return b
	}
	b.fields[k] = strings.TrimSpace(value)
	return b
}

// SetAmount sets a monetary amount and currency material field.
// The amount is normalized to two decimal places (e.g. "1250.50|USD").
func (b *FingerprintBuilder) SetAmount(key string, amount float64, currency string) *FingerprintBuilder {
	k := strings.TrimSpace(key)
	if k == "" {
		return b
	}
	cur := strings.ToUpper(strings.TrimSpace(currency))
	b.fields[k] = fmt.Sprintf("%.2f|%s", amount, cur)
	return b
}

// SetAmountPrecision sets a monetary amount with caller-specified decimal precision.
func (b *FingerprintBuilder) SetAmountPrecision(key string, amount float64, currency string, precision int) *FingerprintBuilder {
	k := strings.TrimSpace(key)
	if k == "" {
		return b
	}
	cur := strings.ToUpper(strings.TrimSpace(currency))
	format := fmt.Sprintf("%%.%df|%%s", precision)
	b.fields[k] = fmt.Sprintf(format, amount, cur)
	return b
}

// AddCollection adds a collection of items (such as line hashes, child IDs, or sub-fingerprints).
// The collection is sorted lexicographically during canonicalization to ensure order-independence.
func (b *FingerprintBuilder) AddCollection(key string, items []string) *FingerprintBuilder {
	k := strings.TrimSpace(key)
	if k == "" {
		return b
	}
	cleaned := make([]string, 0, len(items))
	for _, it := range items {
		trimmed := strings.TrimSpace(it)
		if trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	sort.Strings(cleaned)
	b.collections[k] = cleaned
	return b
}

// Build generates the canonical SHA-256 subject fingerprint string.
func (b *FingerprintBuilder) Build() string {
	h := sha256.New()

	// 1. Sort scalar field keys
	fieldKeys := make([]string, 0, len(b.fields))
	for k := range b.fields {
		fieldKeys = append(fieldKeys, k)
	}
	sort.Strings(fieldKeys)

	for _, k := range fieldKeys {
		// Canonical format: k=v\n
		fmt.Fprintf(h, "%s=%s\n", k, b.fields[k])
	}

	// 2. Sort collection keys
	collKeys := make([]string, 0, len(b.collections))
	for k := range b.collections {
		collKeys = append(collKeys, k)
	}
	sort.Strings(collKeys)

	for _, k := range collKeys {
		items := b.collections[k]
		fmt.Fprintf(h, "@%s=%s\n", k, strings.Join(items, ","))
	}

	sum := h.Sum(nil)
	return FingerprintPrefix + hex.EncodeToString(sum)
}

// ComputeFingerprint is a helper that computes a canonical fingerprint directly
// from a map of material fields. Keys are sorted lexicographically.
func ComputeFingerprint(fields map[string]string) string {
	b := NewFingerprintBuilder()
	for k, v := range fields {
		b.Set(k, v)
	}
	return b.Build()
}

// ComputeCompositeFingerprint computes a fingerprint over an ordered slice of parts.
// Used when domain components have an intrinsic natural sequence (e.g. ProposalID|Status|Gross|Net).
func ComputeCompositeFingerprint(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte("|"))
		}
		h.Write([]byte(strings.TrimSpace(p)))
	}
	sum := h.Sum(nil)
	return FingerprintPrefix + hex.EncodeToString(sum)
}
