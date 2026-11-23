package domain

import (
	"errors"
	"time"
)

// Classification is a Security Standard data class, as sent on the wire by
// the tokenization clients ("S3_RESTRICTED").
type Classification string

// The vault accepts only the classes the standard says to tokenize or
// encrypt. The standard's other classes are deliberately absent:
//
//   - S0 Public / S1 Internal need no tokenization, and accepting them would
//     fill an S3-grade vault with data that never needed one.
//   - S4 Crown-Jewel requires "HSM/external key control ... dual control,
//     exceptional access only". A vault under one configured software key
//     (internal/crypto) does not meet that, so S4 is refused rather than
//     accepted under a weaker control than the standard requires.
const (
	ClassConfidential Classification = "S2_CONFIDENTIAL"
	ClassRestricted   Classification = "S3_RESTRICTED"
)

// Tokenizable reports whether the vault accepts values of class c.
func (c Classification) Tokenizable() bool {
	return c == ClassConfidential || c == ClassRestricted
}

// MaxTokenizableValueBytes bounds one value. Bank account numbers, IBANs and
// tax identifiers are tens of bytes; this is a ceiling against abuse of the
// vault as general storage, not a format rule.
const MaxTokenizableValueBytes = 1024

// TokenizedValue is one vault row. EncryptedValue is ciphertext from
// internal/crypto; the raw value is never held here.
type TokenizedValue struct {
	TokenID              string
	TenantID             string
	LegalEntityID        string
	EncryptedValue       []byte
	Classification       Classification
	CreatedByPrincipalID string
	IdempotencyKey       string
	CreatedAt            time.Time
}

// ErrTokenNotFound means no token with that ID is visible to the caller's
// tenant: it does not exist, or it belongs to another tenant. The vault
// cannot tell those apart — RLS hides other tenants' rows — and callers must
// not try to.
var ErrTokenNotFound = errors.New("token not found")
