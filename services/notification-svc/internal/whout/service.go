package whout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/eventing/egress"
)

// Service runs the INT-04 commands. It contains no authorization: the handler
// authorizes each command against the subscription's legal entity before calling it.
// What it does enforce is everything that must hold however it is called: the state
// machine, separation of duties, optimistic locking, egress and region policy, and the
// confirmation of the signing key reference.
type Service struct {
	store          Store
	keys           KeyVerifier
	egress         egress.Policy
	allowedRegions []string
	now            func() time.Time
	newID          func() string
}

// ServiceConfig wires a Service.
type ServiceConfig struct {
	Store          Store
	Keys           KeyVerifier
	Egress         egress.Policy
	AllowedRegions []string
	Now            func() time.Time
	NewID          func() string
}

// NewService builds a Service.
func NewService(c ServiceConfig) (*Service, error) {
	if c.Store == nil || c.Keys == nil {
		return nil, errors.New("whout: a store and a key verifier are required")
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.NewID == nil {
		c.NewID = uuid.NewString
	}
	regions := make([]string, 0, len(c.AllowedRegions))
	for _, r := range c.AllowedRegions {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
			regions = append(regions, r)
		}
	}
	return &Service{store: c.Store, keys: c.Keys, egress: c.Egress, allowedRegions: regions, now: c.Now, newID: c.NewID}, nil
}

// checkDestination is the full egress check: syntax, residency, and DNS. It reads the
// network, so it runs on a snapshot before any transaction (never inside one) and is
// repeated on every transition that makes a destination live and before every send,
// because DNS answers change (INT-NP-018).
func (s *Service) checkDestination(ctx context.Context, spec Spec) error {
	if !s.regionAllowed(spec.DestinationRegion) {
		return fmt.Errorf("%w: the destination region is not allowed in this deployment", ErrEgressDenied)
	}
	if _, err := s.egress.CheckDestination(ctx, spec.DestinationURL); err != nil {
		if errors.Is(err, egress.ErrDenied) {
			return fmt.Errorf("%w: %v", ErrEgressDenied, err)
		}
		return fmt.Errorf("%w: %v", ErrDependency, err)
	}
	return nil
}

func (s *Service) regionAllowed(region string) bool {
	for _, r := range s.allowedRegions {
		if r == region {
			return true
		}
	}
	return false
}

func (s *Service) caller(a Actor) VaultCaller {
	return VaultCaller{PrincipalID: a.PrincipalID, TenantID: a.TenantID, CorrelationID: a.CorrelationID}
}

func (s *Service) checkKey(ctx context.Context, a Actor, path string) error {
	return s.keys.Verify(ctx, path, s.caller(a))
}

// Create starts a subscription in DRAFT. Nothing is sent to it until a different
// person approves exactly this configuration.
func (s *Service) Create(ctx context.Context, a Actor, spec Spec) (*Subscription, error) {
	spec = spec.Normalize()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.egress.ValidateSyntax(spec.DestinationURL); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEgressDenied, err)
	}
	if !s.regionAllowed(spec.DestinationRegion) {
		return nil, fmt.Errorf("%w: the destination region is not allowed in this deployment", ErrEgressDenied)
	}
	sub := &Subscription{
		SubscriptionID: s.newID(), TenantID: a.TenantID, LegalEntityID: spec.LegalEntityID, Name: spec.Name,
		DestinationURL: spec.DestinationURL, EventTypes: spec.EventTypes, AllowedFields: spec.AllowedFields,
		APIVersion: spec.APIVersion, SigningKeyPath: spec.SigningKeyPath, DestinationRegion: spec.DestinationRegion,
		MaxAttempts: spec.MaxAttempts, Status: StatusDraft, Version: 1, ConfigFingerprint: spec.Fingerprint(),
		CreatedBy: a.PrincipalID, LastChangedBy: a.PrincipalID,
	}
	return s.store.CreateSubscription(ctx, sub, LogEntry{Kind: LogCreated, Actor: a.PrincipalID, ToStatus: StatusDraft, CorrelationID: a.CorrelationID})
}

// Patch is a partial update; nil leaves a field as it is.
type Patch struct {
	Name           *string
	DestinationURL *string
	EventTypes     *[]string
	AllowedFields  *[]string
	MaxAttempts    *int
	Region         *string
}

// Update changes a subscription. A change to anything an approver approves
// (destination, events, fields, API version, region) withdraws the approval: a live or
// pending subscription goes back to DRAFT and needs a new submit and a different
// approver. A change to the name or the retry budget does not.
func (s *Service) Update(ctx context.Context, a Actor, id string, expectedVersion int, p Patch) (*Subscription, error) {
	cur, err := s.store.GetSubscription(ctx, a.TenantID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status == StatusRevoked {
		return nil, fmt.Errorf("%w: the subscription is revoked", ErrInvalidState)
	}
	spec := SpecOf(cur)
	if p.Name != nil {
		spec.Name = *p.Name
	}
	if p.DestinationURL != nil {
		spec.DestinationURL = *p.DestinationURL
	}
	if p.EventTypes != nil {
		spec.EventTypes = *p.EventTypes
	}
	if p.AllowedFields != nil {
		spec.AllowedFields = *p.AllowedFields
	}
	if p.MaxAttempts != nil {
		spec.MaxAttempts = *p.MaxAttempts
	}
	if p.Region != nil {
		spec.DestinationRegion = *p.Region
	}
	spec = spec.Normalize()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.egress.ValidateSyntax(spec.DestinationURL); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEgressDenied, err)
	}
	if !s.regionAllowed(spec.DestinationRegion) {
		return nil, fmt.Errorf("%w: the destination region is not allowed in this deployment", ErrEgressDenied)
	}
	next := *cur
	next.Name, next.DestinationURL, next.EventTypes, next.AllowedFields = spec.Name, spec.DestinationURL, spec.EventTypes, spec.AllowedFields
	next.MaxAttempts, next.DestinationRegion = spec.MaxAttempts, spec.DestinationRegion
	fp := spec.Fingerprint()
	protected := fp != cur.ConfigFingerprint
	reason := ""
	if protected {
		next.ConfigFingerprint, next.LastChangedBy = fp, a.PrincipalID
		next.ApprovedFingerprint, next.ApprovedBy, next.ApprovedAt = "", "", nil
		if cur.Status != StatusDraft {
			next.Status = StatusDraft
			reason = "a protected change withdrew the approval"
			next.StatusReason = reason
		}
	}
	ch := Change{TenantID: a.TenantID, Sub: &next, ExpectedVersion: expectedVersion, Log: LogEntry{
		Kind: LogUpdated, Actor: a.PrincipalID, FromStatus: cur.Status, Reason: reason, CorrelationID: a.CorrelationID}}
	return s.store.ApplyChange(ctx, ch)
}

// Submit asks for approval. The destination and the signing key reference are checked
// now so the approver is never asked to approve something that cannot work.
func (s *Service) Submit(ctx context.Context, a Actor, id string, expectedVersion int) (*Subscription, error) {
	cur, err := s.store.GetSubscription(ctx, a.TenantID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != StatusDraft {
		return nil, fmt.Errorf("%w: only a draft can be submitted", ErrInvalidState)
	}
	if err := s.checkDestination(ctx, SpecOf(cur)); err != nil {
		return nil, err
	}
	if err := s.checkKey(ctx, a, cur.SigningKeyPath); err != nil {
		return nil, err
	}
	next := *cur
	next.Status, next.StatusReason = StatusPendingApproval, ""
	return s.store.ApplyChange(ctx, Change{TenantID: a.TenantID, Sub: &next, ExpectedVersion: expectedVersion, Log: LogEntry{
		Kind: LogSubmitted, Actor: a.PrincipalID, FromStatus: cur.Status, CorrelationID: a.CorrelationID}})
}

// Approve makes a pending subscription ACTIVE. The approver must not be the person who
// last changed the configuration, and the destination, region and signing key are
// checked again now, as the approver (INT-NP: invalid destination approval).
func (s *Service) Approve(ctx context.Context, a Actor, id string, expectedVersion int, reason string) (*Subscription, error) {
	cur, err := s.store.GetSubscription(ctx, a.TenantID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != StatusPendingApproval {
		return nil, fmt.Errorf("%w: only a subscription pending approval can be approved", ErrInvalidState)
	}
	if strings.EqualFold(cur.LastChangedBy, a.PrincipalID) {
		return nil, ErrSelfApproval
	}
	spec := SpecOf(cur)
	if spec.Fingerprint() != cur.ConfigFingerprint {
		return nil, ErrNotApproved
	}
	if err := s.checkDestination(ctx, spec); err != nil {
		return nil, err
	}
	if err := s.checkKey(ctx, a, cur.SigningKeyPath); err != nil {
		return nil, err
	}
	now := s.now()
	next := *cur
	next.Status, next.StatusReason = StatusActive, ""
	next.ApprovedFingerprint, next.ApprovedBy, next.ApprovedAt = cur.ConfigFingerprint, a.PrincipalID, &now
	ev, err := ActivatedEvent(&next, a.PrincipalID, a.CorrelationID)
	if err != nil {
		return nil, err
	}
	return s.store.ApplyChange(ctx, Change{TenantID: a.TenantID, Sub: &next, ExpectedVersion: expectedVersion, ResetHealth: true, Event: &ev,
		Log: LogEntry{Kind: LogApproved, Actor: a.PrincipalID, FromStatus: cur.Status, Reason: reason, CorrelationID: a.CorrelationID}})
}

// Reject returns a pending subscription to DRAFT with a reason.
func (s *Service) Reject(ctx context.Context, a Actor, id string, expectedVersion int, reason string) (*Subscription, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required", ErrValidation)
	}
	cur, err := s.store.GetSubscription(ctx, a.TenantID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != StatusPendingApproval {
		return nil, fmt.Errorf("%w: only a subscription pending approval can be rejected", ErrInvalidState)
	}
	next := *cur
	next.Status, next.StatusReason = StatusDraft, "rejected: "+reason
	return s.store.ApplyChange(ctx, Change{TenantID: a.TenantID, Sub: &next, ExpectedVersion: expectedVersion, Log: LogEntry{
		Kind: LogRejected, Actor: a.PrincipalID, FromStatus: cur.Status, Reason: reason, CorrelationID: a.CorrelationID}})
}

// MaxKeyGrace bounds how long the previous key keeps signing after a rotation.
const MaxKeyGrace = 30 * 24 * time.Hour

// RotateSigningKey points the subscription at a new signing key reference. The old
// reference keeps signing alongside the new one for the grace period, so a recipient
// that has not yet switched keeps verifying (see signature.Sign: both signatures are
// sent). The key itself never passes through here; only the reference does.
func (s *Service) RotateSigningKey(ctx context.Context, a Actor, id string, expectedVersion int, newPath string, grace time.Duration) (*Subscription, error) {
	newPath = strings.TrimSpace(newPath)
	if !keyPathPattern.MatchString(newPath) {
		return nil, fmt.Errorf("%w: new_signing_key_path is not a valid secret path", ErrValidation)
	}
	if grace <= 0 {
		grace = 24 * time.Hour
	}
	if grace > MaxKeyGrace {
		return nil, fmt.Errorf("%w: the grace period is at most %d days", ErrValidation, int(MaxKeyGrace/(24*time.Hour)))
	}
	cur, err := s.store.GetSubscription(ctx, a.TenantID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status == StatusRevoked {
		return nil, fmt.Errorf("%w: the subscription is revoked", ErrInvalidState)
	}
	if newPath == cur.SigningKeyPath {
		return nil, fmt.Errorf("%w: the subscription already uses that key", ErrValidation)
	}
	if err := s.checkKey(ctx, a, newPath); err != nil {
		return nil, err
	}
	until := s.now().Add(grace)
	next := *cur
	next.PreviousSigningKeyPath, next.PreviousKeyValidUntil, next.SigningKeyPath = cur.SigningKeyPath, &until, newPath
	return s.store.ApplyChange(ctx, Change{TenantID: a.TenantID, Sub: &next, ExpectedVersion: expectedVersion, Log: LogEntry{
		Kind: LogKeyRotated, Actor: a.PrincipalID, FromStatus: cur.Status, Reason: "key reference rotated", CorrelationID: a.CorrelationID}})
}

// Suspend pauses an ACTIVE subscription. Its deliveries stay queued.
func (s *Service) Suspend(ctx context.Context, a Actor, id string, expectedVersion int, reason string) (*Subscription, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required", ErrValidation)
	}
	cur, err := s.store.GetSubscription(ctx, a.TenantID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != StatusActive {
		return nil, fmt.Errorf("%w: only an active subscription can be suspended", ErrInvalidState)
	}
	next := *cur
	next.Status, next.StatusReason = StatusSuspended, reason
	return s.store.ApplyChange(ctx, Change{TenantID: a.TenantID, Sub: &next, ExpectedVersion: expectedVersion, Log: LogEntry{
		Kind: LogSuspended, Actor: a.PrincipalID, FromStatus: cur.Status, Reason: reason, CorrelationID: a.CorrelationID}})
}

// Resume returns a suspended or disabled subscription to ACTIVE. Its approval is still
// the approval of its current configuration (the database requires it), so no second
// person is needed, but the destination and key are checked again first.
func (s *Service) Resume(ctx context.Context, a Actor, id string, expectedVersion int, reason string) (*Subscription, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required", ErrValidation)
	}
	cur, err := s.store.GetSubscription(ctx, a.TenantID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != StatusSuspended && cur.Status != StatusDisabled {
		return nil, fmt.Errorf("%w: only a suspended or disabled subscription can be resumed", ErrInvalidState)
	}
	if cur.ApprovedFingerprint == "" || cur.ApprovedFingerprint != cur.ConfigFingerprint {
		return nil, ErrNotApproved
	}
	if err := s.checkDestination(ctx, SpecOf(cur)); err != nil {
		return nil, err
	}
	if err := s.checkKey(ctx, a, cur.SigningKeyPath); err != nil {
		return nil, err
	}
	next := *cur
	next.Status, next.StatusReason = StatusActive, ""
	return s.store.ApplyChange(ctx, Change{TenantID: a.TenantID, Sub: &next, ExpectedVersion: expectedVersion, ResetHealth: true, Log: LogEntry{
		Kind: LogResumed, Actor: a.PrincipalID, FromStatus: cur.Status, Reason: reason, CorrelationID: a.CorrelationID}})
}

// Revoke ends a subscription for good. Its undelivered deliveries go to the dead
// letters with the reason SUBSCRIPTION_REVOKED.
func (s *Service) Revoke(ctx context.Context, a Actor, id string, expectedVersion int, reason string) (*Subscription, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required", ErrValidation)
	}
	cur, err := s.store.GetSubscription(ctx, a.TenantID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status == StatusRevoked {
		return nil, fmt.Errorf("%w: the subscription is already revoked", ErrInvalidState)
	}
	next := *cur
	next.Status, next.StatusReason = StatusRevoked, reason
	ev, err := DisabledEvent(&next, a.PrincipalID, a.CorrelationID, "revoked: "+reason)
	if err != nil {
		return nil, err
	}
	return s.store.ApplyChange(ctx, Change{TenantID: a.TenantID, Sub: &next, ExpectedVersion: expectedVersion, Event: &ev,
		DeadLetterPending: true, DeadLetterReason: ReasonSubscriptionEnded,
		Log: LogEntry{Kind: LogRevoked, Actor: a.PrincipalID, FromStatus: cur.Status, Reason: reason, CorrelationID: a.CorrelationID}})
}

// ReplayApproval is who approved a replay and why.
type ReplayApproval struct {
	Approver    string
	ApprovalRef string
	Reason      string
}

// Replay puts a dead-lettered delivery back on the queue with a fresh retry budget. It
// keeps the delivery's identity, so the recipient can still recognise a duplicate. A
// replay is privileged: it needs a different approver, an approval reference and a
// reason, all recorded (INT-NP: replay).
func (s *Service) Replay(ctx context.Context, a Actor, deliveryID string, ap ReplayApproval) (*Delivery, error) {
	if strings.TrimSpace(ap.Approver) == "" || strings.TrimSpace(ap.ApprovalRef) == "" || strings.TrimSpace(ap.Reason) == "" ||
		strings.EqualFold(strings.TrimSpace(ap.Approver), a.PrincipalID) {
		return nil, ErrReplayUnapproved
	}
	d, err := s.store.GetDelivery(ctx, a.TenantID, deliveryID)
	if err != nil {
		return nil, err
	}
	if d.Status != DeliveryDeadLettered {
		return nil, ErrReplayNotAllowed
	}
	sub, err := s.store.GetSubscription(ctx, a.TenantID, d.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if sub.Status != StatusActive {
		return nil, fmt.Errorf("%w: the subscription is not active", ErrInvalidState)
	}
	ev, err := ReplayEvent(sub, d, a.PrincipalID, ap.Approver, a.CorrelationID, ap.Reason)
	if err != nil {
		return nil, err
	}
	return s.store.Replay(ctx, a.TenantID, deliveryID, LogEntry{
		Kind: LogReplayRequested, Actor: a.PrincipalID, Approver: strings.TrimSpace(ap.Approver), ApprovalRef: strings.TrimSpace(ap.ApprovalRef),
		FromStatus: sub.Status, ToStatus: sub.Status, Version: sub.Version, Fingerprint: sub.ConfigFingerprint, DeliveryID: d.DeliveryID,
		Reason: ap.Reason + " (was dead-lettered: " + d.DeadLetterReason + ")", CorrelationID: a.CorrelationID,
	}, &ev, s.now())
}

// ── queries ─────────────────────────────────────────────────────────────────

func (s *Service) Get(ctx context.Context, tenantID, id string) (*Subscription, error) {
	return s.store.GetSubscription(ctx, tenantID, id)
}

func (s *Service) List(ctx context.Context, tenantID string, status Status, limit int) ([]Subscription, error) {
	return s.store.ListSubscriptions(ctx, tenantID, status, limit)
}

func (s *Service) Log(ctx context.Context, tenantID, id string, limit int) ([]LogEntry, error) {
	if _, err := s.store.GetSubscription(ctx, tenantID, id); err != nil {
		return nil, err
	}
	return s.store.ListLog(ctx, tenantID, id, limit)
}

func (s *Service) Deliveries(ctx context.Context, tenantID, subscriptionID string, status DeliveryStatus, limit int) ([]Delivery, error) {
	return s.store.ListDeliveries(ctx, tenantID, subscriptionID, status, limit)
}

// DeadLetters lists dead-lettered deliveries, optionally for one subscription.
func (s *Service) DeadLetters(ctx context.Context, tenantID, subscriptionID string, limit int) ([]Delivery, error) {
	return s.store.ListDeliveries(ctx, tenantID, subscriptionID, DeliveryDeadLettered, limit)
}

// Evidence is a delivery with its attempts: the destination response evidence.
type Evidence struct {
	Delivery *Delivery
	Attempts []Attempt
}

func (s *Service) Evidence(ctx context.Context, tenantID, deliveryID string) (*Evidence, error) {
	d, err := s.store.GetDelivery(ctx, tenantID, deliveryID)
	if err != nil {
		return nil, err
	}
	at, err := s.store.ListDeliveryAttempts(ctx, tenantID, deliveryID)
	if err != nil {
		return nil, err
	}
	return &Evidence{Delivery: d, Attempts: at}, nil
}

func (s *Service) Health(ctx context.Context, tenantID, subscriptionID string) (*Health, error) {
	return s.store.Health(ctx, tenantID, subscriptionID, s.now())
}
