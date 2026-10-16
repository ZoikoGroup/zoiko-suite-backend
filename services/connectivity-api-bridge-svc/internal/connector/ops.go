package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/store"
)

// ── Ingest ────────────────────────────────────────────────────────────────────

// IngestInput is one ingestion request.
type IngestInput struct {
	PayloadSummary string
	// Scope is the connector scope being exercised; empty means "ingest".
	Scope string
}

// Ingest records that a payload arrived through a connector. It is refused
// unless the connector is live, the scope was approved, the consent has not
// lapsed, and the pinned provider profile is still supported. Where the refusal
// is itself a fact about the connector (consent lapsed, profile withdrawn) the
// connector's health is updated in the same transaction, so the refusal is not
// lost.
func (s *Service) Ingest(ctx context.Context, a Actor, id string, in IngestInput) (*domain.IngestionLog, error) {
	snap, err := s.Store.GetBridgeByID(ctx, id)
	if err != nil {
		return nil, err
	}
	var prof *domain.Profile
	profileMissing := false
	if snap.ProviderKey != "" && snap.ProviderProfileVersion > 0 {
		prof, err = s.Store.GetProfile(ctx, snap.ProviderKey, snap.ProviderProfileVersion)
		switch {
		case errors.Is(err, domain.ErrProfileNotFound):
			profileMissing = true
		case err != nil:
			return nil, fmt.Errorf("%w: %v", ErrDependencyUnavailable, err)
		}
	}
	scope := strings.ToLower(strings.TrimSpace(in.Scope))
	if scope == "" {
		scope = "ingest"
	}

	var refusal error
	log := &domain.IngestionLog{PayloadSummary: in.PayloadSummary, IngestionStatus: domain.IngestionSuccess}
	_, err = s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, h *domain.Health) (*domain.Effects, error) {
		if !b.Live() {
			refusal = ErrNotLive
			return nil, nil
		}
		if !b.HasScope(scope) {
			refusal = ErrScopeDenied
			return nil, nil
		}
		now := s.now()
		if b.ConsentExpiresAt != nil && !b.ConsentExpiresAt.After(now) {
			refusal = ErrCredentialExpired
			return s.degrade(b, h, a, domain.ReasonCredentialExpired, "consent or credential has expired", false), nil
		}
		if b.ProviderKey != "" {
			switch {
			case profileMissing || (prof != nil && prof.Status == domain.ProfileUnsupported):
				refusal = ErrProfileUnavailable
				return s.degrade(b, h, a, domain.ReasonProfileUnsupported, "the pinned provider profile is no longer supported", true), nil
			case prof != nil && prof.Status == domain.ProfileDeprecated:
				// Still served, but the connector must be moved: flag it once.
				fx := s.degrade(b, h, a, domain.ReasonProfileDeprecated, "the pinned provider profile is deprecated", true)
				if fx == nil {
					fx = &domain.Effects{}
				}
				fx.Ingestion = log
				fx.Events = append(fx.Events, ingestedEvent(b, a))
				return fx, nil
			}
		}
		return &domain.Effects{Ingestion: log, Events: []domain.OutboxEvent{ingestedEvent(b, a)}}, nil
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	return log, nil
}

func ingestedEvent(b *domain.ApiBridge, a Actor) domain.OutboxEvent {
	return domain.OutboxEvent{Type: "bridge.ingested", LegacyType: "connectivity.bridge.ingested",
		ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
		Data: map[string]any{"connector_id": b.BridgeID, "scope_checked": true}}
}

// degrade moves a live connector to DEGRADED and records why. It returns nil
// when there is nothing new to record (already degraded for the same reason).
// upgradeRequired also raises connector_profile_upgrade_required.
func (s *Service) degrade(b *domain.ApiBridge, h *domain.Health, a Actor, reason, detail string, upgradeRequired bool) *domain.Effects {
	if h.State == domain.HealthDegraded && h.ReasonCode == reason {
		return nil
	}
	from := b.Version
	if b.Status == domain.StatusActive {
		_ = b.MarkDegraded(reason, s.now())
	}
	fx := &domain.Effects{
		Health: &domain.HealthUpdate{State: domain.HealthDegraded, ReasonCode: reason, Detail: detail, ConsecutiveFailures: h.ConsecutiveFailures + 1},
		Events: []domain.OutboxEvent{{Type: "connector.degraded", ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
			Data: eventData(b, map[string]any{"reason_code": reason})}},
	}
	if b.Version != from {
		fx.Change = &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeDegrade, ChangedBy: a.PrincipalID, Reason: reason}
	}
	if upgradeRequired {
		fx.Events = append(fx.Events, domain.OutboxEvent{Type: "connector.profile-upgrade-required", ActorID: a.PrincipalID,
			CorrelationID: a.CorrelationID, Data: eventData(b, map[string]any{"reason_code": reason})})
	}
	return fx
}

// ── Health ────────────────────────────────────────────────────────────────────

// HealthReport is an observation from whatever runs the connector's syncs.
type HealthReport struct {
	Success bool
	Reason  string
	Detail  string
}

var knownReasons = map[string]bool{
	domain.ReasonCredentialExpired: true, domain.ReasonCredentialRevoked: true, domain.ReasonConsentExpiring: true,
	domain.ReasonProfileDeprecated: true, domain.ReasonProfileUnsupported: true, domain.ReasonProviderOutage: true,
	domain.ReasonIngestionFailures: true, domain.ReasonResidencyConflict: true, domain.ReasonOperatorReported: true,
}

// ReportHealth records a health observation and applies its consequence.
// A revoked credential or a residency conflict stops the connector (nothing may
// continue on authority that is gone); other persistent failures degrade it.
// Checkpoints are never touched: they survive suspension untouched.
func (s *Service) ReportHealth(ctx context.Context, a Actor, id string, rep HealthReport) (*domain.Health, error) {
	reason := strings.ToUpper(strings.TrimSpace(rep.Reason))
	if !rep.Success {
		if reason == "" {
			reason = domain.ReasonOperatorReported
		}
		if !knownReasons[reason] {
			return nil, fmt.Errorf("%w: reason is not a known health reason code", ErrValidation)
		}
	}
	detail := rep.Detail
	if len(detail) > 255 {
		detail = detail[:255]
	}
	var out domain.Health
	_, err := s.Store.MutateConnector(ctx, id, func(b *domain.ApiBridge, h *domain.Health) (*domain.Effects, error) {
		if b.Status == domain.StatusRevoked || b.Status == domain.StatusArchived {
			return nil, domain.ErrConnectorTerminal
		}
		if rep.Success {
			fx := &domain.Effects{Health: &domain.HealthUpdate{State: domain.HealthHealthy, ReasonCode: domain.ReasonRecovered, Success: true}}
			// Only a degradation that health itself caused clears on a healthy
			// report. A lapsed credential or a withdrawn profile does not
			// recover by being polled: it needs a refresh or an upgrade.
			if b.Status == domain.StatusDegraded && !hardReason(h.ReasonCode) {
				from := b.Version
				if err := b.Recover(s.now()); err == nil {
					fx.Change = &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeRecover, ChangedBy: a.PrincipalID}
				}
			} else if b.Status == domain.StatusDegraded {
				fx.Health = &domain.HealthUpdate{State: domain.HealthDegraded, ReasonCode: h.ReasonCode, Detail: h.Detail,
					ConsecutiveFailures: h.ConsecutiveFailures, Success: true}
			}
			next := *h
			next.State = fx.Health.State
			out = next
			return fx, nil
		}

		failures := h.ConsecutiveFailures + 1
		fx := &domain.Effects{Health: &domain.HealthUpdate{State: domain.HealthDegraded, ReasonCode: reason, Detail: detail, ConsecutiveFailures: failures}}
		switch {
		case reason == domain.ReasonCredentialRevoked || reason == domain.ReasonResidencyConflict:
			// Authority is gone: stop, keep everything else.
			fx.Health.State = domain.HealthUnhealthy
			if b.Live() {
				from := b.Version
				if err := b.Suspend(reason, s.now()); err == nil {
					fx.Change = &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeSuspend, ChangedBy: a.PrincipalID, Reason: reason}
					fx.Events = append(fx.Events,
						domain.OutboxEvent{Type: "connector.degraded", ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
							Data: eventData(b, map[string]any{"reason_code": reason})},
						domain.OutboxEvent{Type: "connector.suspended", ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
							Data: eventData(b, map[string]any{"reason": reason})})
				}
			}
		case hardReason(reason) || failures >= domain.FailureThreshold:
			if b.Status == domain.StatusActive {
				from := b.Version
				if err := b.MarkDegraded(reason, s.now()); err == nil {
					fx.Change = &domain.Change{FromVersion: from, ToVersion: b.Version, Kind: domain.ChangeDegrade, ChangedBy: a.PrincipalID, Reason: reason}
				}
			}
			if h.State != domain.HealthDegraded || h.ReasonCode != reason {
				fx.Events = append(fx.Events, domain.OutboxEvent{Type: "connector.degraded", ActorID: a.PrincipalID,
					CorrelationID: a.CorrelationID, Data: eventData(b, map[string]any{"reason_code": reason})})
			}
		default:
			// Below the threshold: record, do not yet degrade.
			fx.Health.State = h.State
			if h.State == "" {
				fx.Health.State = domain.HealthHealthy
			}
		}
		out = domain.Health{ConnectorID: b.BridgeID, TenantID: b.TenantID, State: fx.Health.State, ReasonCode: reason,
			Detail: detail, ConsecutiveFailures: failures}
		return fx, nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// hardReason reports reasons that degrade immediately and do not recover just
// because a later poll succeeds.
func hardReason(r string) bool {
	switch r {
	case domain.ReasonCredentialExpired, domain.ReasonCredentialRevoked, domain.ReasonProfileDeprecated,
		domain.ReasonProfileUnsupported, domain.ReasonResidencyConflict:
		return true
	}
	return false
}

// ── Sync checkpoints ──────────────────────────────────────────────────────────

func validateCheckpoint(stream, cursor string) error {
	if !domain.ValidStream(stream) {
		return fmt.Errorf("%w: stream must be a lower-case identifier", ErrValidation)
	}
	if cursor == "" || len(cursor) > domain.MaxCursorLen {
		return fmt.Errorf("%w: cursor must be between 1 and %d characters", ErrValidation, domain.MaxCursorLen)
	}
	return nil
}

// AdvanceCheckpoint moves a stream forward. expectedVersion is the checkpoint
// version the caller last saw (0 to create the stream); a mismatch means
// someone else advanced it, and nothing is written.
func (s *Service) AdvanceCheckpoint(ctx context.Context, a Actor, id, stream, cursor string, watermark *time.Time, expectedVersion int64) (*domain.Checkpoint, error) {
	if err := validateCheckpoint(stream, cursor); err != nil {
		return nil, err
	}
	data := map[string]any{"connector_id": id, "stream": stream}
	if watermark != nil {
		data["watermark"] = watermark.UTC().Format(time.RFC3339Nano)
	}
	return s.Store.AdvanceCheckpoint(ctx, id, stream, cursor, watermark, expectedVersion, a.PrincipalID,
		domain.OutboxEvent{Type: "sync.checkpoint-advanced", ActorID: a.PrincipalID, CorrelationID: a.CorrelationID, Data: data})
}

// RequestCheckpointReset asks for a checkpoint to be moved back. Nothing moves
// until a different person approves it.
func (s *Service) RequestCheckpointReset(ctx context.Context, a Actor, id, stream, toCursor string, toWatermark *time.Time, reason string) (*domain.ResetRequest, error) {
	if err := validateCheckpoint(stream, toCursor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 255 {
		return nil, fmt.Errorf("%w: a reason of at most 255 characters is required", ErrValidation)
	}
	r := &domain.ResetRequest{ConnectorID: id, Stream: stream, ToCursor: toCursor, ToWatermark: toWatermark, Reason: reason, RequestedBy: a.PrincipalID}
	err := s.Store.CreateResetRequest(ctx, r, domain.OutboxEvent{Type: "sync.checkpoint-reset-requested", ActorID: a.PrincipalID,
		CorrelationID: a.CorrelationID, Data: map[string]any{"connector_id": id, "stream": stream, "reason": reason}})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// DecideCheckpointReset approves or rejects a reset. The decider must not be
// the requester.
func (s *Service) DecideCheckpointReset(ctx context.Context, a Actor, requestID string, approve bool) (*domain.ResetRequest, error) {
	typ := "sync.checkpoint-reset-rejected"
	if approve {
		typ = "sync.checkpoint-reset-applied"
	}
	return s.Store.DecideResetRequest(ctx, requestID, a.PrincipalID, approve,
		domain.OutboxEvent{Type: typ, ActorID: a.PrincipalID, CorrelationID: a.CorrelationID, Data: map[string]any{"request_id": requestID}})
}

// ── Provider profiles ─────────────────────────────────────────────────────────

// PublishProfile registers a new provider profile version as a DRAFT. Only the
// platform scope may.
func (s *Service) PublishProfile(ctx context.Context, tenant string, a Actor, p *domain.Profile) error {
	if tenant != store.PlatformScopeID {
		return ErrNotPlatformScope
	}
	p.ProviderKey = strings.ToLower(strings.TrimSpace(p.ProviderKey))
	p.ProviderType = strings.ToUpper(strings.TrimSpace(p.ProviderType))
	p.AllowedRegions = lowerAll(p.AllowedRegions)
	p.AllowedHostSuffixes = lowerAll(p.AllowedHostSuffixes)
	p.SupportedScopes = domain.NormalizeScopes(p.SupportedScopes)
	p.PublishedBy, p.Status = a.PrincipalID, domain.ProfileDraft
	if err := p.Validate(); err != nil {
		return err
	}
	return s.Store.PublishProfile(ctx, p, domain.OutboxEvent{Type: "external-profile.published", ActorID: a.PrincipalID,
		CorrelationID: a.CorrelationID, Data: map[string]any{"provider_key": p.ProviderKey, "version": p.Version, "provider_type": p.ProviderType}})
}

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// SetProfileStatus certifies, deprecates or withdraws a profile version. It
// only moves forward. Connectors pinned to it find out the next time they are
// used or checked; none is moved silently (INT-CTRL-025).
func (s *Service) SetProfileStatus(ctx context.Context, tenant string, a Actor, key string, version int, status string) (*domain.Profile, error) {
	if tenant != store.PlatformScopeID {
		return nil, ErrNotPlatformScope
	}
	status = strings.ToUpper(strings.TrimSpace(status))
	var typ string
	switch status {
	case domain.ProfileCertified:
		typ = "external-profile.certified"
	case domain.ProfileDeprecated:
		typ = "external-profile.deprecated"
	case domain.ProfileUnsupported:
		typ = "external-profile.unsupported"
	default:
		return nil, fmt.Errorf("%w: status must be CERTIFIED, DEPRECATED or UNSUPPORTED", ErrValidation)
	}
	return s.Store.SetProfileStatus(ctx, strings.ToLower(key), version, status, s.now(),
		domain.OutboxEvent{Type: typ, ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
			Data: map[string]any{"provider_key": strings.ToLower(key), "version": version, "status": status}})
}
