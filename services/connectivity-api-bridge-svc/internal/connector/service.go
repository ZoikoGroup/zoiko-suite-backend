// Package connector holds the Connector Management commands (ZS-SVC-M-001
// INT-05): create, change, submit, approve, activate, suspend, revoke, refresh
// consent, upgrade profile, report health, and the sync-checkpoint and provider
// profile commands.
//
// ORDER OF WORK. Anything that talks to the outside world (the provider
// profile registry, DNS, the secret vault) is done against a snapshot BEFORE the
// database transaction opens. The transaction then re-checks that the connector
// is still the version that was checked and applies the change. No network
// call ever runs while a row is locked or a pooled connection is held.
package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/egress"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
)

// Errors the handler maps to stable INT-* problems.
var (
	ErrProfileUnavailable    = errors.New("the pinned provider profile is not available for this action")
	ErrRegionDenied          = errors.New("the destination region is not permitted")
	ErrHostNotAllowed        = errors.New("the destination host is not allowlisted for this provider")
	ErrScopeNotSupported     = errors.New("a requested scope is not supported by the provider profile")
	ErrScopeDenied           = errors.New("the connector's approved scopes do not permit that")
	ErrEgressDenied          = errors.New("the destination is not an allowed egress target")
	ErrCredentialInvalid     = errors.New("the credential reference could not be confirmed")
	ErrCredentialExpired     = errors.New("the connector's consent or credential has expired")
	ErrDependencyUnavailable = errors.New("a dependency needed to check this change is unavailable")
	ErrNotLive               = errors.New("the connector is not active")
	ErrValidation            = errors.New("the request is not valid")
	ErrNotPlatformScope      = errors.New("only the platform scope may do that")
)

// Actor is who is acting.
type Actor struct {
	PrincipalID   string
	CorrelationID string
}

// Service runs the connector commands.
type Service struct {
	Store store.Store
	Vault vault.Verifier
	// Egress is the destination policy. EgressAllowedRegions are the regions this
	// deployment may send data to; EMPTY MEANS NONE (fail closed).
	Egress               egress.Policy
	EgressAllowedRegions []string
	Now                  func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) caller(ctx context.Context, tenant string, a Actor) vault.Caller {
	return vault.Caller{PrincipalID: a.PrincipalID, TenantID: tenant, CorrelationID: a.CorrelationID}
}

// ── Create ────────────────────────────────────────────────────────────────────

// CreateInput is a create request after the handler has decoded it.
type CreateInput struct {
	domain.CreateBridgeRequest
	TenantID       string
	IdempotencyKey string
}

// Create registers a connector as a DRAFT. A draft may be incomplete; nothing
// that could be a credential is accepted even here.
func (s *Service) Create(ctx context.Context, a Actor, in CreateInput) (*domain.ApiBridge, bool, error) {
	name := firstNonEmpty(in.BridgeName, in.ProviderName)
	endpoint := firstNonEmpty(in.EndpointURL, in.BaseURL)
	if strings.TrimSpace(in.LegalEntityID) == "" || strings.TrimSpace(name) == "" {
		return nil, false, fmt.Errorf("%w: legal_entity_id and bridge_name/provider_name are required", ErrValidation)
	}
	b := &domain.ApiBridge{
		LegalEntityID: in.LegalEntityID, BridgeName: name, ProviderName: name, Protocol: in.Protocol,
		EndpointURL: endpoint, BaseURL: endpoint, AuthType: in.AuthType, Status: domain.StatusDraft,
		Purpose: in.Purpose, ProviderKey: in.ProviderKey, ProviderProfileVersion: in.ProviderProfileVersion,
		CredentialRef: in.CredentialRef, DestinationRegion: in.DestinationRegion, SyncMode: in.SyncMode,
		Scopes: in.Scopes, Version: 1, CreatedBy: a.PrincipalID, LastChangedBy: a.PrincipalID,
	}
	cfg := b.Protected()
	if msgs := cfg.ValidateSyntax(); len(msgs) > 0 {
		return nil, false, fmt.Errorf("%w: %s", domain.ErrIncompleteConfig, strings.Join(msgs, "; "))
	}
	b.EndpointURL, b.BaseURL = cfg.EndpointURL, cfg.EndpointURL
	b.AuthType, b.SyncMode, b.Scopes = cfg.AuthType, cfg.SyncMode, cfg.Scopes
	b.DestinationRegion, b.ProviderKey, b.CredentialRef, b.Purpose = cfg.DestinationRegion, cfg.ProviderKey, cfg.CredentialRef, cfg.Purpose

	fx := &domain.Effects{
		Change: &domain.Change{FromVersion: 0, ToVersion: 1, Kind: domain.ChangeCreate, ChangedBy: a.PrincipalID,
			ChangedFields: nonEmptyFields(cfg), ScopesAdded: cfg.Scopes},
		Events: []domain.OutboxEvent{{
			Type: "bridge.created", LegacyType: "connectivity.bridge.created", ActorID: a.PrincipalID,
			CorrelationID: a.CorrelationID, Data: eventData(b, nil),
		}},
	}
	hash := idemHash(in.LegalEntityID, name, cfg.Fingerprint())
	created, err := s.Store.CreateConnector(ctx, b, in.IdempotencyKey, hash, fx)
	if err != nil {
		return nil, false, err
	}
	return b, created, nil
}

func nonEmptyFields(c domain.ProtectedConfig) []string {
	var f []string
	add := func(n string, set bool) {
		if set {
			f = append(f, n)
		}
	}
	add("endpoint_url", c.EndpointURL != "")
	add("destination_region", c.DestinationRegion != "")
	add("purpose", c.Purpose != "")
	add("provider_key", c.ProviderKey != "")
	add("provider_profile_version", c.ProviderProfileVersion > 0)
	add("credential_ref", c.CredentialRef != "")
	add("auth_type", c.AuthType != "")
	add("sync_mode", c.SyncMode != "")
	add("scopes", len(c.Scopes) > 0)
	return f
}

func idemHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// eventData is the minimal, secret-free payload every connector event carries.
func eventData(b *domain.ApiBridge, extra map[string]any) map[string]any {
	d := map[string]any{
		"connector_id": b.BridgeID, "status": b.Status, "version": b.Version,
		"provider_key": b.ProviderKey, "provider_profile_version": b.ProviderProfileVersion,
		"destination_region": b.DestinationRegion, "purpose": b.Purpose, "scopes": b.Scopes,
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// ── Change ────────────────────────────────────────────────────────────────────

// Patch is a partial update. A nil field is left alone.
type Patch struct {
	BridgeName             *string
	EndpointURL            *string
	AuthType               *string
	Purpose                *string
	ProviderKey            *string
	ProviderProfileVersion *int
	CredentialRef          *string
	DestinationRegion      *string
	SyncMode               *string
	Scopes                 *[]string
}

func (p Patch) apply(c domain.ProtectedConfig) domain.ProtectedConfig {
	if p.EndpointURL != nil {
		c.EndpointURL = strings.TrimSpace(*p.EndpointURL)
	}
	if p.AuthType != nil {
		c.AuthType = strings.ToUpper(strings.TrimSpace(*p.AuthType))
	}
	if p.Purpose != nil {
		c.Purpose = strings.TrimSpace(*p.Purpose)
	}
	if p.ProviderKey != nil {
		c.ProviderKey = strings.ToLower(strings.TrimSpace(*p.ProviderKey))
	}
	if p.ProviderProfileVersion != nil {
		c.ProviderProfileVersion = *p.ProviderProfileVersion
	}
	if p.CredentialRef != nil {
		c.CredentialRef = strings.ToLower(strings.TrimSpace(*p.CredentialRef))
	}
	if p.DestinationRegion != nil {
		c.DestinationRegion = strings.ToLower(strings.TrimSpace(*p.DestinationRegion))
	}
	if p.SyncMode != nil {
		c.SyncMode = strings.ToUpper(strings.TrimSpace(*p.SyncMode))
	}
	if p.Scopes != nil {
		c.Scopes = domain.NormalizeScopes(*p.Scopes)
	}
	return c
}

// Update applies a patch. A change to any protected field withdraws the
// connector's approval and, if it was live, stops it until the new
// configuration is approved by someone else (INT-NP-021, INT-NP-023).
func (s *Service) Update(ctx context.Context, a Actor, id string, expected int64, p Patch) (*domain.ApiBridge, error) {
	return s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, _ *domain.Health) (*domain.Effects, error) {
		if b.Version != expected {
			return nil, domain.ErrVersionConflict
		}
		prior := b.Protected()
		priorFP := prior.Fingerprint()
		fromVersion := b.Version
		wasLive := b.Live()

		fields, added, removed, err := b.ApplyChange(p.apply(prior), a.PrincipalID, s.now())
		if err != nil {
			return nil, err
		}
		if p.BridgeName != nil && strings.TrimSpace(*p.BridgeName) != "" && *p.BridgeName != b.BridgeName {
			b.BridgeName, b.ProviderName = *p.BridgeName, *p.BridgeName
			fields = append(fields, "bridge_name")
			if len(fields) == 1 { // a rename alone is not a protected change
				b.Version++
				b.UpdatedAt = s.now()
			}
		}
		if len(fields) == 0 {
			return nil, nil
		}
		fx := &domain.Effects{Change: &domain.Change{
			FromVersion: fromVersion, ToVersion: b.Version, Kind: domain.ChangeUpdate, ChangedBy: a.PrincipalID,
			ChangedFields: fields, ScopesAdded: added, ScopesRemoved: removed,
			PriorFingerprint: priorFP, NewFingerprint: b.Protected().Fingerprint(),
		}}
		if len(added) > 0 || len(removed) > 0 {
			fx.Events = append(fx.Events, domain.OutboxEvent{Type: "connector.scope-changed", ActorID: a.PrincipalID,
				CorrelationID: a.CorrelationID, Data: eventData(b, map[string]any{
					"scopes_added": added, "scopes_removed": removed, "was_live": wasLive, "approval_required": true})})
		}
		if containsAny(fields, "endpoint_url", "destination_region", "credential_ref", "provider_key", "provider_profile_version", "purpose", "auth_type", "sync_mode") {
			fx.Events = append(fx.Events, domain.OutboxEvent{Type: "connector.config-changed", ActorID: a.PrincipalID,
				CorrelationID: a.CorrelationID, Data: eventData(b, map[string]any{"changed_fields": fields, "was_live": wasLive})})
		}
		return fx, nil
	})
}

func containsAny(have []string, want ...string) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

// ── External checks ───────────────────────────────────────────────────────────

// checkExternal validates a configuration against everything outside this
// service's own tables. It must run before a transaction opens.
func (s *Service) checkExternal(ctx context.Context, tenant string, a Actor, b *domain.ApiBridge) error {
	cfg := b.Protected()
	if msgs := cfg.ValidateComplete(); len(msgs) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrIncompleteConfig, strings.Join(msgs, "; "))
	}
	prof, err := s.Store.GetProfile(ctx, cfg.ProviderKey, cfg.ProviderProfileVersion)
	switch {
	case errors.Is(err, domain.ErrProfileNotFound):
		return fmt.Errorf("%w: no such provider profile version", ErrProfileUnavailable)
	case err != nil:
		return fmt.Errorf("%w: %v", ErrDependencyUnavailable, err)
	}
	if prof.Status != domain.ProfileCertified {
		return fmt.Errorf("%w: profile is %s; only a CERTIFIED profile may be pinned", ErrProfileUnavailable, strings.ToLower(prof.Status))
	}
	if !prof.AllowsRegion(cfg.DestinationRegion) || !s.regionAllowed(cfg.DestinationRegion) {
		return ErrRegionDenied
	}
	if missing := prof.UnsupportedScopes(cfg.Scopes); len(missing) > 0 {
		return fmt.Errorf("%w: %d scope(s) are not supported by this profile", ErrScopeNotSupported, len(missing))
	}
	host, err := s.Egress.CheckDestination(ctx, cfg.EndpointURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEgressDenied, err)
	}
	if !egress.HostMatchesSuffix(host, prof.AllowedHostSuffixes) {
		return ErrHostNotAllowed
	}
	if cfg.AuthType != "NONE" {
		if err := s.Vault.Verify(ctx, cfg.CredentialRef, s.caller(ctx, tenant, a)); err != nil {
			if errors.Is(err, vault.ErrUnavailable) {
				return fmt.Errorf("%w: secret vault", ErrDependencyUnavailable)
			}
			return fmt.Errorf("%w: %v", ErrCredentialInvalid, err)
		}
	}
	return nil
}

func (s *Service) regionAllowed(region string) bool {
	for _, r := range s.EgressAllowedRegions {
		if strings.EqualFold(r, region) {
			return true
		}
	}
	return false
}

// snapshot reads a connector for checking.
func (s *Service) snapshot(ctx context.Context, id string, expected int64) (*domain.ApiBridge, error) {
	b, err := s.Store.GetBridgeByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if expected != 0 && b.Version != expected {
		return nil, domain.ErrVersionConflict
	}
	return b, nil
}

// ── Lifecycle commands ────────────────────────────────────────────────────────

// Submit puts a complete draft forward for approval.
func (s *Service) Submit(ctx context.Context, tenant string, a Actor, id string, expected int64) (*domain.ApiBridge, error) {
	snap, err := s.snapshot(ctx, id, expected)
	if err != nil {
		return nil, err
	}
	if snap.Status != domain.StatusDraft {
		return nil, domain.ErrInvalidTransition
	}
	if err := s.checkExternal(ctx, tenant, a, snap); err != nil {
		return nil, err
	}
	return s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, _ *domain.Health) (*domain.Effects, error) {
		if b.Version != snap.Version {
			return nil, domain.ErrVersionConflict
		}
		from := b.Version
		if err := b.Submit(a.PrincipalID, s.now()); err != nil {
			return nil, err
		}
		t := s.now()
		b.CredentialVerifiedAt = credVerified(b, &t)
		return &domain.Effects{
			Change: &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeSubmit, ChangedBy: a.PrincipalID,
				NewFingerprint: b.ConfigFingerprint},
			Events: []domain.OutboxEvent{{Type: "connector.approval-requested", ActorID: a.PrincipalID,
				CorrelationID: a.CorrelationID, Data: eventData(b, map[string]any{"fingerprint": b.ConfigFingerprint})}},
		}, nil
	})
}

func credVerified(b *domain.ApiBridge, t *time.Time) *time.Time {
	if b.AuthType == "NONE" || b.CredentialRef == "" {
		return nil
	}
	return t
}

// Approve records a second person's approval of exactly the submitted
// configuration. Everything is re-checked: the destination may have started
// resolving somewhere else, the profile may have been deprecated, the
// credential may have been retired since it was submitted.
func (s *Service) Approve(ctx context.Context, tenant string, a Actor, id string, expected int64, reason string) (*domain.ApiBridge, error) {
	snap, err := s.snapshot(ctx, id, expected)
	if err != nil {
		return nil, err
	}
	if snap.Status != domain.StatusPendingApproval {
		return nil, domain.ErrInvalidTransition
	}
	if a.PrincipalID == "" || a.PrincipalID == snap.LastChangedBy {
		return nil, domain.ErrSelfApproval
	}
	if err := s.checkExternal(ctx, tenant, a, snap); err != nil {
		return nil, err
	}
	return s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, _ *domain.Health) (*domain.Effects, error) {
		if b.Version != snap.Version {
			return nil, domain.ErrVersionConflict
		}
		from, maker := b.Version, b.LastChangedBy
		if err := b.Approve(a.PrincipalID, s.now()); err != nil {
			return nil, err
		}
		t := s.now()
		b.CredentialVerifiedAt = credVerified(b, &t)
		return &domain.Effects{
			Change: &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeApprove, ChangedBy: a.PrincipalID,
				NewFingerprint: b.ApprovedFingerprint, Reason: reason},
			Approval: &domain.Approval{ConnectorVersion: b.Version, Fingerprint: b.ApprovedFingerprint,
				Decision: domain.DecisionApproved, RequestedBy: maker, DecidedBy: a.PrincipalID, Reason: reason},
			Events: []domain.OutboxEvent{{Type: "connector.approved", ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
				Data: eventData(b, map[string]any{"fingerprint": b.ApprovedFingerprint, "approved_by": a.PrincipalID})}},
		}, nil
	})
}

// Reject returns a pending connector to draft with a reason.
func (s *Service) Reject(ctx context.Context, a Actor, id string, expected int64, reason string) (*domain.ApiBridge, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required to reject", ErrValidation)
	}
	return s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, _ *domain.Health) (*domain.Effects, error) {
		if b.Version != expected {
			return nil, domain.ErrVersionConflict
		}
		from, maker, fp := b.Version, b.LastChangedBy, b.ConfigFingerprint
		if err := b.Reject(a.PrincipalID, reason, s.now()); err != nil {
			return nil, err
		}
		return &domain.Effects{
			Change:   &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeReject, ChangedBy: a.PrincipalID, Reason: reason},
			Approval: &domain.Approval{ConnectorVersion: from, Fingerprint: fp, Decision: domain.DecisionRejected, RequestedBy: maker, DecidedBy: a.PrincipalID, Reason: reason},
			Events: []domain.OutboxEvent{{Type: "connector.rejected", ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
				Data: eventData(b, map[string]any{"reason": reason})}},
		}, nil
	})
}

// Activate makes an approved connector live. It is also how a suspended one
// resumes. The configuration is re-checked against the world as it is now.
func (s *Service) Activate(ctx context.Context, tenant string, a Actor, id string, expected int64) (*domain.ApiBridge, error) {
	snap, err := s.snapshot(ctx, id, expected)
	if err != nil {
		return nil, err
	}
	if snap.Status != domain.StatusPendingApproval && snap.Status != domain.StatusSuspended {
		return nil, domain.ErrInvalidTransition
	}
	if !snap.LegacyUnapproved { // a grandfathered connector has no profile to check
		if err := s.checkExternal(ctx, tenant, a, snap); err != nil {
			return nil, err
		}
	}
	return s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, h *domain.Health) (*domain.Effects, error) {
		if b.Version != snap.Version {
			return nil, domain.ErrVersionConflict
		}
		from := b.Version
		if err := b.Activate(s.now()); err != nil {
			return nil, err
		}
		if !b.LegacyUnapproved {
			t := s.now()
			b.CredentialVerifiedAt = credVerified(b, &t)
		}
		return &domain.Effects{
			Change: &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeActivate, ChangedBy: a.PrincipalID},
			Health: &domain.HealthUpdate{State: domain.HealthHealthy, ReasonCode: domain.ReasonRecovered, Success: true},
			Events: []domain.OutboxEvent{{Type: "connector.activated", ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
				Data: eventData(b, nil)}},
		}, nil
	})
}

// Suspend stops a live connector.
func (s *Service) Suspend(ctx context.Context, a Actor, id string, expected int64, reason string) (*domain.ApiBridge, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required to suspend", ErrValidation)
	}
	return s.transition(ctx, a, id, expected, domain.ChangeSuspend, "connector.suspended", reason,
		func(b *domain.ApiBridge) error { return b.Suspend(reason, s.now()) })
}

// Revoke ends a connector for good.
func (s *Service) Revoke(ctx context.Context, a Actor, id string, expected int64, reason string) (*domain.ApiBridge, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required to revoke", ErrValidation)
	}
	return s.transition(ctx, a, id, expected, domain.ChangeRevoke, "connector.revoked", reason,
		func(b *domain.ApiBridge) error { return b.Revoke(reason, s.now()) })
}

// Archive retires a revoked connector.
func (s *Service) Archive(ctx context.Context, a Actor, id string, expected int64) (*domain.ApiBridge, error) {
	return s.transition(ctx, a, id, expected, domain.ChangeArchive, "connector.archived", "",
		func(b *domain.ApiBridge) error { return b.Archive(s.now()) })
}

func (s *Service) transition(ctx context.Context, a Actor, id string, expected int64, kind, event, reason string,
	apply func(*domain.ApiBridge) error) (*domain.ApiBridge, error) {
	return s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, _ *domain.Health) (*domain.Effects, error) {
		if b.Version != expected {
			return nil, domain.ErrVersionConflict
		}
		from := b.Version
		if err := apply(b); err != nil {
			return nil, err
		}
		return &domain.Effects{
			Change: &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: kind, ChangedBy: a.PrincipalID, Reason: reason},
			Events: []domain.OutboxEvent{{Type: event, ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
				Data: eventData(b, map[string]any{"reason": reason})}},
		}, nil
	})
}

// ── Consent / credential and profile upgrade ──────────────────────────────────

// RefreshConsent records a new credential reference and/or a new consent
// expiry. Extending the expiry of the SAME reference changes nothing that was
// approved, so it needs no re-approval; pointing the connector at a DIFFERENT
// reference is a protected change like any other.
func (s *Service) RefreshConsent(ctx context.Context, tenant string, a Actor, id string, expected int64, newRef *string, expires *time.Time) (*domain.ApiBridge, error) {
	if newRef == nil && expires == nil {
		return nil, fmt.Errorf("%w: a new credential_ref or consent_expires_at is required", ErrValidation)
	}
	if expires != nil && (expires.Before(s.now()) || expires.After(s.now().AddDate(1, 0, 0))) {
		return nil, fmt.Errorf("%w: consent_expires_at must be in the future and within a year", ErrValidation)
	}
	snap, err := s.snapshot(ctx, id, expected)
	if err != nil {
		return nil, err
	}
	ref := snap.CredentialRef
	if newRef != nil {
		ref = strings.ToLower(strings.TrimSpace(*newRef))
	}
	if snap.AuthType == "NONE" {
		return nil, fmt.Errorf("%w: this connector does not use a credential", ErrValidation)
	}
	if !domain.ValidCredentialRef(ref) {
		return nil, fmt.Errorf("%w: credential_ref must be a secret-vault policy id, not a credential", domain.ErrIncompleteConfig)
	}
	if err := s.Vault.Verify(ctx, ref, s.caller(ctx, tenant, a)); err != nil {
		if errors.Is(err, vault.ErrUnavailable) {
			return nil, fmt.Errorf("%w: secret vault", ErrDependencyUnavailable)
		}
		return nil, fmt.Errorf("%w: %v", ErrCredentialInvalid, err)
	}
	return s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, h *domain.Health) (*domain.Effects, error) {
		if b.Version != snap.Version {
			return nil, domain.ErrVersionConflict
		}
		from, wasLive := b.Version, b.Live()
		priorFP := b.Protected().Fingerprint()
		var fields []string
		if newRef != nil && ref != b.CredentialRef {
			cfg := b.Protected()
			cfg.CredentialRef = ref
			f, _, _, err := b.ApplyChange(cfg, a.PrincipalID, s.now())
			if err != nil {
				return nil, err
			}
			fields = f
		} else {
			if b.Status == domain.StatusRevoked || b.Status == domain.StatusArchived {
				return nil, domain.ErrConnectorTerminal
			}
			b.Version++
			b.UpdatedAt = s.now()
		}
		t := s.now()
		b.CredentialVerifiedAt = &t
		if expires != nil {
			b.ConsentExpiresAt = expires
			fields = append(fields, "consent_expires_at")
		}
		fx := &domain.Effects{
			Change: &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeConsent, ChangedBy: a.PrincipalID,
				ChangedFields: fields, PriorFingerprint: priorFP, NewFingerprint: b.Protected().Fingerprint()},
			Events: []domain.OutboxEvent{{Type: "connector.credential-refreshed", ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
				Data: eventData(b, map[string]any{"approval_required": !b.Live() && wasLive, "consent_extended": expires != nil})}},
		}
		// A connector degraded only because its credential lapsed is healthy again.
		if b.Status == domain.StatusDegraded && h.ReasonCode == domain.ReasonCredentialExpired {
			if err := b.Recover(s.now()); err == nil {
				fx.Health = &domain.HealthUpdate{State: domain.HealthHealthy, ReasonCode: domain.ReasonRecovered, Success: true}
			}
		}
		return fx, nil
	})
}

// UpgradeProfile moves a connector to a newer certified version of its
// provider profile. It is never done automatically (INT-CTRL-025): the new
// version is a protected change, so it is approved like any other.
func (s *Service) UpgradeProfile(ctx context.Context, a Actor, id string, expected int64, toVersion int) (*domain.ApiBridge, error) {
	snap, err := s.snapshot(ctx, id, expected)
	if err != nil {
		return nil, err
	}
	if toVersion <= snap.ProviderProfileVersion {
		return nil, fmt.Errorf("%w: a profile upgrade must move to a higher version", ErrValidation)
	}
	prof, err := s.Store.GetProfile(ctx, snap.ProviderKey, toVersion)
	if errors.Is(err, domain.ErrProfileNotFound) {
		return nil, fmt.Errorf("%w: no such provider profile version", ErrProfileUnavailable)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependencyUnavailable, err)
	}
	if prof.Status != domain.ProfileCertified {
		return nil, fmt.Errorf("%w: profile is %s; only a CERTIFIED profile may be pinned", ErrProfileUnavailable, strings.ToLower(prof.Status))
	}
	return s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, h *domain.Health) (*domain.Effects, error) {
		if b.Version != snap.Version {
			return nil, domain.ErrVersionConflict
		}
		from, priorFP, wasLive := b.Version, b.Protected().Fingerprint(), b.Live()
		cfg := b.Protected()
		cfg.ProviderProfileVersion = toVersion
		fields, _, _, err := b.ApplyChange(cfg, a.PrincipalID, s.now())
		if err != nil {
			return nil, err
		}
		return &domain.Effects{
			Change: &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeProfile, ChangedBy: a.PrincipalID,
				ChangedFields: fields, PriorFingerprint: priorFP, NewFingerprint: b.Protected().Fingerprint()},
			Health: &domain.HealthUpdate{State: domain.HealthHealthy, ReasonCode: domain.ReasonRecovered, Success: true},
			Events: []domain.OutboxEvent{{Type: "connector.profile-upgraded", ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
				Data: eventData(b, map[string]any{"to_version": toVersion, "was_live": wasLive, "approval_required": true})}},
		}, nil
	})
}

// ProfileStatus describes the connector's pinned profile (GetProviderProfileStatus).
type ProfileStatus struct {
	ProviderKey     string     `json:"provider_key"`
	PinnedVersion   int        `json:"pinned_version"`
	Status          string     `json:"status"`
	UpgradeRequired bool       `json:"upgrade_required"`
	DeprecatedAt    *time.Time `json:"deprecated_at,omitempty"`
	SunsetAt        *time.Time `json:"sunset_at,omitempty"`
	LatestCertified int        `json:"latest_certified_version,omitempty"`
	AllowedRegions  []string   `json:"allowed_regions,omitempty"`
	SupportedScopes []string   `json:"supported_scopes,omitempty"`
}

// ProfileStatusOf reports on the profile a connector is pinned to.
func (s *Service) ProfileStatusOf(ctx context.Context, id string) (*ProfileStatus, error) {
	b, err := s.Store.GetBridgeByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.ProviderKey == "" || b.ProviderProfileVersion == 0 {
		return &ProfileStatus{Status: "NONE"}, nil
	}
	pinned, err := s.Store.GetProfile(ctx, b.ProviderKey, b.ProviderProfileVersion)
	if errors.Is(err, domain.ErrProfileNotFound) {
		return &ProfileStatus{ProviderKey: b.ProviderKey, PinnedVersion: b.ProviderProfileVersion, Status: "UNKNOWN", UpgradeRequired: true}, nil
	}
	if err != nil {
		return nil, err
	}
	all, err := s.Store.ListProfiles(ctx, b.ProviderKey)
	if err != nil {
		return nil, err
	}
	latest := 0
	for _, p := range all {
		if p.Status == domain.ProfileCertified && p.Version > latest {
			latest = p.Version
		}
	}
	return &ProfileStatus{
		ProviderKey: b.ProviderKey, PinnedVersion: pinned.Version, Status: pinned.Status,
		UpgradeRequired: pinned.Status == domain.ProfileDeprecated || pinned.Status == domain.ProfileUnsupported,
		DeprecatedAt:    pinned.DeprecatedAt, SunsetAt: pinned.SunsetAt, LatestCertified: latest,
		AllowedRegions: pinned.AllowedRegions, SupportedScopes: pinned.SupportedScopes,
	}, nil
}

// HTTPStatus is a convenience for tests that want the status a typed error
// maps to; the handler owns the real mapping.
func HTTPStatus(err error) int {
	switch {
	case errors.Is(err, domain.ErrBridgeNotFound), errors.Is(err, domain.ErrProfileNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrVersionConflict), errors.Is(err, domain.ErrInvalidTransition):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}
