package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Lifecycle errors. The handler maps each to a stable INT-* problem; none of
// them carries caller input.
var (
	ErrInvalidTransition   = errors.New("that action is not valid for the connector's current status")
	ErrVersionConflict     = errors.New("the connector was changed since the version supplied")
	ErrSelfApproval        = errors.New("the person who last changed this connector cannot approve it")
	ErrNotApproved         = errors.New("the connector's current configuration has not been approved")
	ErrIncompleteConfig    = errors.New("the connector configuration is incomplete or invalid")
	ErrConnectorTerminal   = errors.New("a revoked or archived connector cannot be changed")
	ErrIdempotencyConflict = errors.New("the idempotency key was already used for a different request")
)

// ProtectedConfig is the part of a connector whose change must be approved
// again. It is exactly what the approval fingerprint covers (INT-CTRL-014,
// INT-NP-021, INT-NP-023): change the destination, the scopes, the purpose, the
// pinned provider profile or the credential reference and the approval no longer
// describes the connector.
type ProtectedConfig struct {
	EndpointURL            string   `json:"endpoint_url"`
	DestinationRegion      string   `json:"destination_region"`
	Purpose                string   `json:"purpose"`
	ProviderKey            string   `json:"provider_key"`
	ProviderProfileVersion int      `json:"provider_profile_version"`
	CredentialRef          string   `json:"credential_ref"`
	AuthType               string   `json:"auth_type"`
	SyncMode               string   `json:"sync_mode"`
	Scopes                 []string `json:"scopes"`
}

// Protected extracts the protected configuration, normalised so that equal
// configurations compare equal: scopes are sorted and de-duplicated, the auth
// type is upper-cased, the endpoint is trimmed.
func (b *ApiBridge) Protected() ProtectedConfig {
	return ProtectedConfig{
		EndpointURL:            strings.TrimSpace(b.EndpointURL),
		DestinationRegion:      strings.ToLower(strings.TrimSpace(b.DestinationRegion)),
		Purpose:                strings.TrimSpace(b.Purpose),
		ProviderKey:            strings.ToLower(strings.TrimSpace(b.ProviderKey)),
		ProviderProfileVersion: b.ProviderProfileVersion,
		CredentialRef:          strings.ToLower(strings.TrimSpace(b.CredentialRef)),
		AuthType:               strings.ToUpper(strings.TrimSpace(b.AuthType)),
		SyncMode:               strings.ToUpper(strings.TrimSpace(b.SyncMode)),
		Scopes:                 NormalizeScopes(b.Scopes),
	}
}

// NormalizeScopes sorts, de-duplicates and trims scopes.
func NormalizeScopes(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Fingerprint is the digest an approval is bound to. The encoding is the JSON
// of ProtectedConfig, whose field order is fixed by the struct, so the same
// configuration always yields the same digest.
func (c ProtectedConfig) Fingerprint() string {
	b, _ := json.Marshal(c) // a struct of strings, ints and a string slice cannot fail
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Diff names the protected fields that differ (never their values: a changed
// credential reference or URL is not something evidence should repeat) and the
// scopes added and removed.
func (c ProtectedConfig) Diff(next ProtectedConfig) (fields, added, removed []string) {
	add := func(name string, changed bool) {
		if changed {
			fields = append(fields, name)
		}
	}
	add("endpoint_url", c.EndpointURL != next.EndpointURL)
	add("destination_region", c.DestinationRegion != next.DestinationRegion)
	add("purpose", c.Purpose != next.Purpose)
	add("provider_key", c.ProviderKey != next.ProviderKey)
	add("provider_profile_version", c.ProviderProfileVersion != next.ProviderProfileVersion)
	add("credential_ref", c.CredentialRef != next.CredentialRef)
	add("auth_type", c.AuthType != next.AuthType)
	add("sync_mode", c.SyncMode != next.SyncMode)

	old := toSet(c.Scopes)
	cur := toSet(next.Scopes)
	for s := range cur {
		if _, ok := old[s]; !ok {
			added = append(added, s)
		}
	}
	for s := range old {
		if _, ok := cur[s]; !ok {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	add("scopes", len(added) > 0 || len(removed) > 0)
	return fields, added, removed
}

func toSet(in []string) map[string]struct{} {
	m := make(map[string]struct{}, len(in))
	for _, s := range in {
		m[s] = struct{}{}
	}
	return m
}

// ── Validation ───────────────────────────────────────────────────────────────

var (
	tokenPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	purposePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)
	scopePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)
	uuidPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	secretQueryKey  = regexp.MustCompile(`(?i)(api[_-]?key|apikey|token|secret|passw(or)?d|passwd|auth|credential|signature|^sig$|session|bearer)`)
	allowedAuthType = map[string]bool{"NONE": true, "API_KEY": true, "OAUTH2": true, "MTLS": true, "BASIC": true, "HMAC": true}
)

// MaxScopes bounds a connector's scope list.
const MaxScopes = 32

// ValidCredentialRef reports whether ref has the shape of a vault secret-policy
// id. A reference is an identifier; this is what makes it opaque by
// construction: nothing that is not an id can be stored in the field.
func ValidCredentialRef(ref string) bool { return uuidPattern.MatchString(strings.ToLower(ref)) }

// ValidateSyntax checks what must hold even for a DRAFT: nothing that could be
// a secret may be stored, and every value must have a sane shape. It does NOT
// require completeness. It returns one message per problem, naming fields and
// never echoing values.
func (c ProtectedConfig) ValidateSyntax() []string {
	var bad []string
	if c.EndpointURL != "" {
		if msg := ValidateEndpointSyntax(c.EndpointURL); msg != "" {
			bad = append(bad, msg)
		}
	}
	if c.DestinationRegion != "" && !tokenPattern.MatchString(c.DestinationRegion) {
		bad = append(bad, "destination_region must be a region code")
	}
	if c.Purpose != "" && !purposePattern.MatchString(c.Purpose) {
		bad = append(bad, "purpose must be a lower-case identifier of 3 to 64 characters")
	}
	if c.ProviderKey != "" && !tokenPattern.MatchString(c.ProviderKey) {
		bad = append(bad, "provider_key must be a lower-case identifier")
	}
	if c.ProviderProfileVersion < 0 {
		bad = append(bad, "provider_profile_version must not be negative")
	}
	if c.CredentialRef != "" && !ValidCredentialRef(c.CredentialRef) {
		bad = append(bad, "credential_ref must be a secret-vault policy id, not a credential")
	}
	if c.AuthType != "" && !allowedAuthType[c.AuthType] {
		bad = append(bad, "auth_type must be one of NONE, API_KEY, OAUTH2, MTLS, BASIC, HMAC")
	}
	if c.SyncMode != "" && c.SyncMode != SyncPull && c.SyncMode != SyncPush && c.SyncMode != SyncNone {
		bad = append(bad, "sync_mode must be PULL, PUSH or NONE")
	}
	if len(c.Scopes) > MaxScopes {
		bad = append(bad, fmt.Sprintf("at most %d scopes are allowed", MaxScopes))
	}
	for _, s := range c.Scopes {
		if !scopePattern.MatchString(s) {
			bad = append(bad, "every scope must be a lower-case identifier")
			break
		}
	}
	return bad
}

// ValidateComplete checks everything a connector needs before it can be
// approved or used. It includes ValidateSyntax.
func (c ProtectedConfig) ValidateComplete() []string {
	bad := c.ValidateSyntax()
	need := func(ok bool, msg string) {
		if !ok {
			bad = append(bad, msg)
		}
	}
	need(c.EndpointURL != "", "endpoint_url is required")
	need(c.Purpose != "", "purpose is required")
	need(c.ProviderKey != "", "provider_key is required")
	need(c.ProviderProfileVersion > 0, "provider_profile_version is required")
	need(c.DestinationRegion != "", "destination_region is required")
	need(c.AuthType != "", "auth_type is required")
	need(c.SyncMode != "", "sync_mode is required")
	need(len(c.Scopes) > 0, "at least one scope is required")
	if c.AuthType != "" && c.AuthType != "NONE" {
		need(c.CredentialRef != "", "credential_ref is required for this auth_type")
	}
	if c.AuthType == "NONE" {
		need(c.CredentialRef == "", "credential_ref must be empty when auth_type is NONE")
	}
	return bad
}

// ValidateEndpointSyntax checks an endpoint URL's shape and that it carries no
// credential. It returns "" when acceptable.
func ValidateEndpointSyntax(raw string) string {
	if len(raw) > 512 {
		return "endpoint_url is too long"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "endpoint_url must be an absolute URL"
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "endpoint_url must use https"
	}
	if u.User != nil {
		return "endpoint_url must not carry credentials; use credential_ref"
	}
	if u.Fragment != "" {
		return "endpoint_url must not carry a fragment"
	}
	for key := range u.Query() {
		if secretQueryKey.MatchString(key) {
			return "endpoint_url must not carry credentials in its query; use credential_ref"
		}
	}
	return ""
}

// ── The state machine ────────────────────────────────────────────────────────
//
// Every transition below mutates the connector in place, bumps Version and
// UpdatedAt, and returns an error without having changed anything when it is
// not allowed. They are pure: no clock, no I/O, no storage, so each can be
// tested exhaustively. Checks that need the outside world (the vault, the
// profile registry, DNS) happen in the service before the transition is applied.

func (b *ApiBridge) touch(now time.Time) {
	b.Version++
	b.UpdatedAt = now
}

func (b *ApiBridge) terminal() bool {
	return b.Status == StatusRevoked || b.Status == StatusArchived
}

func (b *ApiBridge) clearApproval() {
	b.ApprovedFingerprint = ""
	b.ApprovedVersion = 0
	b.ApprovedBy = ""
	b.ApprovedAt = nil
}

// Live reports whether the connector may be used to move data.
func (b *ApiBridge) Live() bool { return b.Status == StatusActive || b.Status == StatusDegraded }

// ApplyChange applies a protected-configuration change made by actor. The
// connector's approval, if any, no longer describes it, so it is cleared; a
// connector that was live goes back to PENDING_APPROVAL and stops being usable
// until a different person approves the new configuration (INT-NP-021,
// INT-NP-023). Returns the fields that changed.
func (b *ApiBridge) ApplyChange(next ProtectedConfig, actor string, now time.Time) (fields, added, removed []string, err error) {
	if b.terminal() {
		return nil, nil, nil, ErrConnectorTerminal
	}
	current := b.Protected()
	fields, added, removed = current.Diff(next)
	if len(fields) == 0 {
		return nil, nil, nil, nil
	}
	if msgs := next.ValidateSyntax(); len(msgs) > 0 {
		return nil, nil, nil, fmt.Errorf("%w: %s", ErrIncompleteConfig, strings.Join(msgs, "; "))
	}

	b.EndpointURL, b.BaseURL = next.EndpointURL, next.EndpointURL
	b.DestinationHost = EndpointHost(next.EndpointURL)
	b.DestinationRegion = next.DestinationRegion
	b.Purpose = next.Purpose
	b.ProviderKey = next.ProviderKey
	b.ProviderProfileVersion = next.ProviderProfileVersion
	b.CredentialRef = next.CredentialRef
	b.CredentialVerifiedAt = nil // the new reference has not been verified
	b.AuthType = next.AuthType
	b.SyncMode = next.SyncMode
	b.Scopes = next.Scopes
	b.LastChangedBy = actor

	b.ConfigFingerprint = ""
	b.clearApproval()
	b.LegacyUnapproved = false // it is leaving the grandfathered state: it is governed from here

	switch b.Status {
	case StatusDraft:
		// stays a draft
	case StatusPendingApproval:
		// stays pending; the earlier submission no longer applies, so it must
		// be submitted again with the new configuration
		b.Status = StatusDraft
		b.StatusReason = "configuration changed while awaiting approval; submit again"
	default: // ACTIVE, DEGRADED, SUSPENDED
		b.Status = StatusDraft
		b.StatusReason = "protected configuration changed; submit for approval"
	}
	b.touch(now)
	return fields, added, removed, nil
}

// EndpointHost returns the lower-cased host of a URL without port, or "".
func EndpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Submit moves a complete DRAFT to PENDING_APPROVAL and fixes the fingerprint
// the approver will be approving.
func (b *ApiBridge) Submit(actor string, now time.Time) error {
	if b.Status != StatusDraft {
		return ErrInvalidTransition
	}
	cfg := b.Protected()
	if msgs := cfg.ValidateComplete(); len(msgs) > 0 {
		return fmt.Errorf("%w: %s", ErrIncompleteConfig, strings.Join(msgs, "; "))
	}
	b.ConfigFingerprint = cfg.Fingerprint()
	b.Status = StatusPendingApproval
	b.StatusReason = ""
	b.touch(now)
	return nil
}

// Approve records an approval of exactly the configuration that was submitted.
// The approver must be someone other than whoever last changed it.
func (b *ApiBridge) Approve(approver string, now time.Time) error {
	if b.Status != StatusPendingApproval {
		return ErrInvalidTransition
	}
	if approver == "" || approver == b.LastChangedBy {
		return ErrSelfApproval
	}
	if b.Protected().Fingerprint() != b.ConfigFingerprint {
		// The row's own configuration no longer matches the one submitted.
		return ErrNotApproved
	}
	b.ApprovedFingerprint = b.ConfigFingerprint
	b.ApprovedVersion = b.Version
	b.ApprovedBy = approver
	t := now
	b.ApprovedAt = &t
	b.touch(now)
	return nil
}

// Reject returns a pending connector to DRAFT with the reason, clearing any
// approval. The rejector must also be someone other than the maker.
func (b *ApiBridge) Reject(rejector, reason string, now time.Time) error {
	if b.Status != StatusPendingApproval {
		return ErrInvalidTransition
	}
	if rejector == "" || rejector == b.LastChangedBy {
		return ErrSelfApproval
	}
	b.clearApproval()
	b.ConfigFingerprint = ""
	b.Status = StatusDraft
	b.StatusReason = reason
	b.touch(now)
	return nil
}

// Activate makes an approved connector live. It re-derives the fingerprint from
// the configuration as it is now and refuses unless that is exactly what was
// approved, so a configuration changed behind the approval's back cannot go
// live. It also reactivates a SUSPENDED connector whose approval still stands.
func (b *ApiBridge) Activate(now time.Time) error {
	if b.Status != StatusPendingApproval && b.Status != StatusSuspended {
		return ErrInvalidTransition
	}
	// A grandfathered connector that an operator suspended may be resumed as it
	// was: it never had an approval to lose. Anything else needs one that still
	// describes the configuration.
	legacyResume := b.LegacyUnapproved && b.Status == StatusSuspended
	if !legacyResume && (b.ApprovedFingerprint == "" || b.Protected().Fingerprint() != b.ApprovedFingerprint ||
		b.ConfigFingerprint != b.ApprovedFingerprint) {
		return ErrNotApproved
	}
	b.Status = StatusActive
	b.StatusReason = ""
	b.touch(now)
	return nil
}

// Suspend stops a live connector without losing its approval or checkpoints.
func (b *ApiBridge) Suspend(reason string, now time.Time) error {
	if !b.Live() {
		return ErrInvalidTransition
	}
	b.Status = StatusSuspended
	b.StatusReason = reason
	b.touch(now)
	return nil
}

// MarkDegraded records that a live connector is impaired but not stopped.
func (b *ApiBridge) MarkDegraded(reason string, now time.Time) error {
	if b.Status != StatusActive {
		return ErrInvalidTransition
	}
	b.Status = StatusDegraded
	b.StatusReason = reason
	b.touch(now)
	return nil
}

// Recover returns a degraded connector to ACTIVE. Its configuration did not
// change, so no approval is needed.
func (b *ApiBridge) Recover(now time.Time) error {
	if b.Status != StatusDegraded {
		return ErrInvalidTransition
	}
	b.Status = StatusActive
	b.StatusReason = ""
	b.touch(now)
	return nil
}

// Revoke ends a connector for good. Revocation is terminal: it is how consent
// withdrawal and decommissioning are recorded without losing history.
func (b *ApiBridge) Revoke(reason string, now time.Time) error {
	if b.terminal() {
		return ErrConnectorTerminal
	}
	b.Status = StatusRevoked
	b.StatusReason = reason
	b.touch(now)
	return nil
}

// Archive retires a revoked connector from active listings.
func (b *ApiBridge) Archive(now time.Time) error {
	if b.Status != StatusRevoked {
		return ErrInvalidTransition
	}
	b.Status = StatusArchived
	b.touch(now)
	return nil
}

// HasScope reports whether scope is among the connector's approved scopes.
func (b *ApiBridge) HasScope(scope string) bool {
	scope = strings.ToLower(strings.TrimSpace(scope))
	for _, s := range b.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}
