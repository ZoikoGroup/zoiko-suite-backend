package federation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Provisioning is INT-10's directory side: governed mapping from directory attributes to baseline roles, SCIM
// user provisioning with deduplicated events, prioritised deprovisioning with a latency objective, and
// reconciliation. It never grants authority. It asks access-control-svc for baseline assignments (which refuses
// anything privileged), and it ends access first and asks questions afterwards.
type Provisioning struct {
	repo  ProvisionRepo
	conns Repo
	ac    AccessControl
	sess  SessionEvictor
	log   *zap.Logger
	now   func() time.Time

	// tuning, with production defaults
	BackoffBase       time.Duration
	BackoffMax        time.Duration
	RevokeBackoffMax  time.Duration
	ExceptionAfter    int
	SettleTimeout     time.Duration
	SweepBatch        int
	Lease             time.Duration
	ReconcileMaxShare float64
}

// NewProvisioning wires the provisioning service. Every dependency is required: a deployment that cannot end
// sessions or revoke assignments must not accept deprovisioning it cannot carry out.
func NewProvisioning(repo ProvisionRepo, conns Repo, ac AccessControl, sess SessionEvictor, log *zap.Logger) (*Provisioning, error) {
	if repo == nil || conns == nil || ac == nil || sess == nil {
		return nil, errors.New("provisioning needs a store, access control and a session evictor")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Provisioning{repo: repo, conns: conns, ac: ac, sess: sess, log: log, now: time.Now,
		BackoffBase: 10 * time.Second, BackoffMax: 15 * time.Minute, RevokeBackoffMax: time.Minute, ExceptionAfter: 5,
		SettleTimeout: 8 * time.Second, SweepBatch: 50, Lease: time.Minute, ReconcileMaxShare: 0.25}, nil
}

// SetClock replaces the clock; for tests.
func (p *Provisioning) SetClock(now func() time.Time) { p.now = now }

// ── administration ───────────────────────────────────────────────────────────

// checkEligibility asks access control whether every role a mapping names is baseline. Fail closed: a role
// whose eligibility cannot be established is not eligible, and the caller learns it was a question of
// availability rather than of the role.
func (p *Provisioning) checkEligibility(ctx context.Context, tenantID string, cfg MappingConfig) error {
	for _, role := range cfg.RoleIDs() {
		el, err := p.ac.RoleEligibility(ctx, tenantID, role)
		if err != nil {
			if errors.Is(err, ErrAccessControlUnavailable) {
				return fmt.Errorf("%w: the eligibility of role %s could not be checked", ErrAccessControlUnavailable, role)
			}
			return fmt.Errorf("%w: the eligibility of role %s could not be established", ErrAccessControlUnavailable, role)
		}
		if !el.Eligible {
			return fmt.Errorf("%w: role %s cannot be mapped from a directory (%s); privileged access is requested through governance approval",
				ErrInvalid, role, el.Reason)
		}
	}
	return nil
}

// ConfigureMapping is ConfigureProvisioningMapping: it proposes a mapping version, which awaits an
// independent approver before it affects anyone.
func (p *Provisioning) ConfigureMapping(ctx context.Context, tenantID, actor, correlationID, connectionID string, cfg MappingConfig) (*MappingVersion, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := p.checkEligibility(ctx, tenantID, cfg); err != nil {
		return nil, err
	}
	return p.repo.CreateMappingVersion(ctx, tenantID, connectionID, cfg, actor, correlationID)
}

// ApproveMapping approves exactly the fingerprint the approver saw. Eligibility is checked again: a role may
// have gained privileged actions since the mapping was proposed.
func (p *Provisioning) ApproveMapping(ctx context.Context, tenantID, actor, correlationID, connectionID string, version int, fingerprint, reason string) (*MappingVersion, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	_, all, err := p.repo.GetMappings(ctx, tenantID, connectionID)
	if err != nil {
		return nil, err
	}
	for _, m := range all {
		if m.Version == version {
			if m.Status == MapPending {
				if err := p.checkEligibility(ctx, tenantID, m.Config); err != nil {
					return nil, err
				}
			}
			break
		}
	}
	return p.repo.DecideMappingVersion(ctx, tenantID, connectionID, version, true, actor, fingerprint, strings.TrimSpace(reason), correlationID)
}

// RejectMapping declines a proposed mapping version.
func (p *Provisioning) RejectMapping(ctx context.Context, tenantID, actor, correlationID, connectionID string, version int, reason string) (*MappingVersion, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required to reject", ErrInvalid)
	}
	return p.repo.DecideMappingVersion(ctx, tenantID, connectionID, version, false, actor, "", strings.TrimSpace(reason), correlationID)
}

// MappingState is the answer to a mapping query.
type MappingState struct {
	Active   *MappingVersion  `json:"active"`
	Versions []MappingVersion `json:"versions"`
}

// GetMapping returns the mapping versions of a connection.
func (p *Provisioning) GetMapping(ctx context.Context, tenantID, connectionID string) (*MappingState, error) {
	a, all, err := p.repo.GetMappings(ctx, tenantID, connectionID)
	if err != nil {
		return nil, err
	}
	if all == nil {
		all = []MappingVersion{}
	}
	return &MappingState{Active: a, Versions: all}, nil
}

// IssueSCIMToken creates the connection's SCIM bearer token, revoking the previous one. The token is returned
// once; only its hash is kept.
func (p *Provisioning) IssueSCIMToken(ctx context.Context, tenantID, actor, correlationID, connectionID string) (string, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := "zscim_" + base64.RawURLEncoding.EncodeToString(raw)
	if err := p.repo.CreateSCIMToken(ctx, tenantID, connectionID, hashToken(token), actor, correlationID); err != nil {
		return "", err
	}
	return token, nil
}

// RevokeSCIMToken revokes the connection's SCIM token.
func (p *Provisioning) RevokeSCIMToken(ctx context.Context, tenantID, actor, correlationID, connectionID string) (int, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return 0, err
	}
	return p.repo.RevokeSCIMTokens(ctx, tenantID, connectionID, actor, correlationID)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// ── queries ──────────────────────────────────────────────────────────────────

// Status is GetProvisioningStatus.
func (p *Provisioning) Status(ctx context.Context, tenantID, connectionID string) (*ProvStatus, error) {
	return p.repo.ProvisioningStatus(ctx, tenantID, connectionID)
}

// Events lists the provisioning ledger.
func (p *Provisioning) Events(ctx context.Context, tenantID, connectionID, status string, limit int) ([]ProvEvent, error) {
	return p.repo.ListEvents(ctx, tenantID, connectionID, status, limit)
}

// Exceptions is ListSyncExceptions.
func (p *Provisioning) Exceptions(ctx context.Context, tenantID, connectionID, status string, limit int) ([]Exception, error) {
	return p.repo.ListExceptions(ctx, tenantID, connectionID, status, limit)
}

// ResolveException closes an exception with a note: a human's statement that it has been dealt with.
func (p *Provisioning) ResolveException(ctx context.Context, tenantID, actor, exceptionID, resolution string) (*Exception, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(resolution) == "" {
		return nil, fmt.Errorf("%w: a resolution note is required", ErrInvalid)
	}
	return p.repo.ResolveException(ctx, tenantID, exceptionID, actor, strings.TrimSpace(resolution))
}

// Latency is GetDeprovisionLatency: how long deprovisioning took to take effect, against its objective.
func (p *Provisioning) Latency(ctx context.Context, tenantID, connectionID string, limit int) (*DeprovisionLatency, error) {
	rows, err := p.repo.DeprovisionLatency(ctx, tenantID, connectionID, limit)
	if err != nil {
		return nil, err
	}
	out := &DeprovisionLatency{Rows: []LatencyRow{}}
	var done []int64
	for i := range rows {
		r := &rows[i]
		if r.AppliedAt != nil {
			ms := r.AppliedAt.Sub(r.RequestedAt).Milliseconds()
			r.LocalMillis = &ms
		}
		if r.SessionsEvictedAt != nil && r.AccessRevokedAt != nil {
			end := *r.SessionsEvictedAt
			if r.AccessRevokedAt.After(end) {
				end = *r.AccessRevokedAt
			}
			ms := end.Sub(r.RequestedAt).Milliseconds()
			r.CompleteMillis = &ms
			done = append(done, ms)
			out.Completed++
			if r.DueAt != nil && end.After(*r.DueAt) {
				out.SLOExceeded++
			}
		} else {
			out.Pending++
		}
		if r.Escalated {
			out.Escalated++
		}
		out.Rows = append(out.Rows, *r)
	}
	if len(done) > 0 {
		sortInt64(done)
		out.MedianMS, out.MaxMS = done[len(done)/2], done[len(done)-1]
	}
	return out, nil
}

func sortInt64(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// ── SCIM ─────────────────────────────────────────────────────────────────────

// SCIMTarget is an authenticated SCIM request's connection context.
type SCIMTarget struct {
	TenantID string
	Conn     Connection
	Ver      Version
	Mapping  *MappingVersion
}

// AuthenticateSCIM checks a bearer token against the connection's active token and loads what the request
// needs. Every failure is the same ErrTokenInvalid: nothing about the connection is revealed to a caller
// without a valid token, and nothing durable is written for one.
func (p *Provisioning) AuthenticateSCIM(ctx context.Context, tenantID, connectionID, bearer string) (*SCIMTarget, error) {
	if tenantID == "" || connectionID == "" || bearer == "" || len(bearer) > 200 {
		return nil, ErrTokenInvalid
	}
	ok, err := p.repo.ValidSCIMToken(ctx, tenantID, connectionID, hashToken(bearer))
	if err != nil {
		return nil, fmt.Errorf("scim token lookup: %w", err)
	}
	if !ok {
		return nil, ErrTokenInvalid
	}
	d, err := p.conns.GetConnection(ctx, tenantID, connectionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrTokenInvalid
		}
		return nil, err
	}
	switch d.Status {
	case ConnActive, ConnDegraded, ConnSuspended:
	default:
		return nil, ErrTokenInvalid
	}
	ver := activeVersion(d)
	if ver == nil {
		return nil, ErrTokenInvalid
	}
	active, _, err := p.repo.GetMappings(ctx, tenantID, connectionID)
	if err != nil {
		return nil, err
	}
	return &SCIMTarget{TenantID: tenantID, Conn: d.Connection, Ver: *ver, Mapping: active}, nil
}

// SCIMRequest is the common envelope of a mutating SCIM call.
type SCIMRequest struct {
	Method, Path string
	Body         []byte
	// EventID is the sender's own identity for the event (X-Event-Id / Idempotency-Key), if it sent one.
	EventID       string
	CorrelationID string
}

// dedupeKey returns the ledger identity of a request. A sender-supplied event id is exact and permanent. Without
// one the key is the request's content bound to a five-minute window: a directory's retry of a request it did
// not hear back on is absorbed, while the same instruction legitimately repeated later (deactivate, reactivate,
// deactivate) is a new event.
func (p *Provisioning) dedupeKey(r SCIMRequest) (key, payloadHash string) {
	sum := sha256.Sum256([]byte(r.Method + " " + r.Path + "\n" + string(r.Body)))
	payloadHash = hex.EncodeToString(sum[:])
	if id := strings.TrimSpace(r.EventID); id != "" {
		if len(id) > 150 {
			id = hex.EncodeToString(sha256Sum(id))
		}
		return "id:" + id, payloadHash
	}
	return fmt.Sprintf("h:%s:%d", payloadHash[:32], p.now().Unix()/300), payloadHash
}

func sha256Sum(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

type coreRequest struct {
	Op          string
	Source      string
	LinkSource  string
	Subject     string
	User        *SCIMUser
	ExpectedRev *int
	DedupeKey   string
	PayloadHash string
	Corr        string
	Actor       string
}

// Outcome is the result of a provisioning request.
type Outcome struct {
	Event    ProvEvent
	Result   *ApplyResult
	View     *UserView
	Replayed bool
}

func mappingConfig(t *SCIMTarget) (MappingConfig, int) {
	if t.Mapping == nil {
		return MappingConfig{}.Normalize(), 0
	}
	return t.Mapping.Config, t.Mapping.Version
}

// rejection is a decided refusal, stored on the event so a duplicate gets the same answer.
func rejection(status int, scimType, detail string) *SCIMError {
	return &SCIMError{Status: status, Type: scimType, Detail: detail}
}

func (p *Provisioning) provision(ctx context.Context, t *SCIMTarget, c coreRequest) (*Outcome, error) {
	ev, created, err := p.repo.BeginEvent(ctx, ProvEvent{TenantID: t.TenantID, ConnectionID: t.Conn.ConnectionID, DedupeKey: c.DedupeKey,
		Source: c.Source, Operation: c.Op, Subject: c.Subject, PayloadHash: c.PayloadHash})
	if err != nil {
		return nil, fmt.Errorf("provisioning ledger: %w", err)
	}
	if !created {
		if ev.PayloadHash != c.PayloadHash {
			return nil, ErrDedupeMismatch
		}
		switch ev.Status {
		case ProvApplied:
			id, _ := ev.Result["link_id"].(string)
			view, verr := p.repo.GetUser(ctx, t.TenantID, t.Conn.ConnectionID, id)
			if verr != nil {
				return nil, verr
			}
			return &Outcome{Event: *ev, View: view, Replayed: true}, nil
		case ProvRejected:
			return nil, storedRejection(ev)
		}
		// RECEIVED / VALIDATED: an earlier attempt did not finish; apply again (apply is idempotent)
	}

	reject := func(e *SCIMError, reason, exception string) (*Outcome, error) {
		res := map[string]any{"scim_status": e.Status, "scim_type": e.Type, "detail": e.Detail}
		if derr := p.repo.DecideEvent(ctx, t.TenantID, ev.EventID, ProvRejected, reason, exception, res); derr != nil {
			p.log.Error("could not record a rejected provisioning event", zap.String("event_id", ev.EventID), zap.Error(derr))
		}
		return nil, e
	}

	// a suspended connection admits no new access, but it never refuses to take access away
	deactivating := c.Op == OpDeprovision || (c.User != nil && !c.User.Active)
	if t.Conn.Status == ConnSuspended && !deactivating {
		return reject(rejection(http.StatusForbidden, "", "federation is suspended for this connection"), "connection_suspended", "")
	}

	cfg, mver := mappingConfig(t)
	in := ApplyInput{
		TenantID: t.TenantID, ConnectionID: t.Conn.ConnectionID, EventID: ev.EventID, Source: c.Source, Issuer: t.Ver.Config.Issuer,
		Operation: c.Op, Subject: c.Subject, Active: !deactivating, MappingVersion: mver, ExpectedRev: c.ExpectedRev,
		Actor: c.Actor, CorrelationID: c.Corr, ReceivedAt: ev.ReceivedAt, LinkSource: c.LinkSource,
		DeprovisionSLO: time.Duration(cfg.DeprovisionSLOSeconds) * time.Second,
	}
	ignored := 0
	if c.User != nil {
		in.Username, in.Email, in.DisplayName, in.Attributes = c.User.UserName, c.User.Email, c.User.DisplayName, c.User.Attrs
		if !deactivating {
			desired, ign, ok := Evaluate(cfg, c.User.Attrs)
			if !ok {
				return reject(rejection(http.StatusConflict, "tooMany",
					"the record matches more roles than the mapping allows one person; it was quarantined for review"), "ambiguous_mapping", ExcAmbiguousMapping)
			}
			in.Desired, ignored = desired, ign
		}
	}

	res, err := p.repo.ApplyIdentity(ctx, in)
	switch {
	case errors.Is(err, ErrAmbiguousIdentity):
		return reject(rejection(http.StatusConflict, "uniqueness",
			"another person in this tenant already uses that email address; it was quarantined for review"), "ambiguous_identity", ExcAmbiguousIdentity)
	case errors.Is(err, ErrUserNotFound):
		return reject(rejection(http.StatusNotFound, "", "no such user"), "user_not_found", "")
	case errors.Is(err, ErrDeprovisionInProgress):
		// transient: leave the event open so the directory's retry applies it once the earlier deprovisioning is done
		return nil, rejection(http.StatusConflict, "", "the earlier deprovisioning of this user is still in progress; retry shortly")
	case errors.Is(err, ErrStaleLink):
		return nil, ErrStaleLink
	case err != nil:
		return nil, fmt.Errorf("apply provisioning: %w", err)
	}

	p.afterApply(ctx, t.TenantID, t.Conn.ConnectionID, res)

	view, verr := p.repo.GetUser(ctx, t.TenantID, t.Conn.ConnectionID, res.Link.LinkID)
	if verr != nil {
		return nil, verr
	}
	result := map[string]any{"link_id": res.Link.LinkID, "principal_id": res.Link.PrincipalID, "created": res.Created,
		"reactivated": res.Reactivated, "deprovisioned": res.Deprovisioned, "unchanged": res.Unchanged,
		"planned": res.Planned, "revoking": res.Revoking, "ignored_values": ignored}
	if derr := p.repo.DecideEvent(ctx, t.TenantID, ev.EventID, ProvApplied, "", "", result); derr != nil {
		p.log.Error("could not record an applied provisioning event", zap.String("event_id", ev.EventID), zap.Error(derr))
	}
	ev.Status, ev.Result = ProvApplied, result
	return &Outcome{Event: *ev, Result: res, View: view}, nil
}

func storedRejection(ev *ProvEvent) error {
	status, _ := ev.Result["scim_status"].(float64)
	typ, _ := ev.Result["scim_type"].(string)
	detail, _ := ev.Result["detail"].(string)
	if status == 0 {
		status = http.StatusConflict
	}
	return &SCIMError{Status: int(status), Type: typ, Detail: detail}
}

// afterApply does what must follow a decision, best effort and in priority order: end the person's sessions,
// then drive their queued assignment work. Anything it cannot finish stays queued for the worker, which
// retries and escalates; nothing here can be lost, and nothing here can make the decision itself fail.
func (p *Provisioning) afterApply(ctx context.Context, tenantID, connectionID string, res *ApplyResult) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.SettleTimeout)
	defer cancel()
	if res.Link.Status == LinkDeprovisioned && res.Link.SessionsEvictedAt == nil {
		p.evict(actx, tenantID, res.Link.PrincipalID, res.Link.LinkID)
	}
	p.settleLink(actx, tenantID, res.Link.LinkID)
	if res.Link.Status == LinkDeprovisioned {
		if _, err := p.repo.MarkAccessRevokedIfDone(actx, tenantID, res.Link.LinkID); err != nil {
			p.log.Warn("could not mark deprovisioning complete", zap.String("link_id", res.Link.LinkID), zap.Error(err))
		}
	}
}

func (p *Provisioning) evict(ctx context.Context, tenantID, principalID, linkID string) bool {
	if _, err := p.sess.EvictAllForPrincipal(ctx, principalID, tenantID); err != nil {
		p.log.Error("could not end a deprovisioned person's sessions; the worker will retry",
			zap.String("principal_id", principalID), zap.String("tenant_id", tenantID), zap.Error(err))
		return false
	}
	if err := p.repo.MarkSessionsEvicted(ctx, tenantID, linkID); err != nil {
		p.log.Error("could not record that sessions were ended", zap.String("link_id", linkID), zap.Error(err))
		return false
	}
	return true
}

func (p *Provisioning) settleLink(ctx context.Context, tenantID, linkID string) {
	rows, err := p.repo.PendingAssignments(ctx, tenantID, linkID)
	if err != nil {
		p.log.Warn("could not read queued assignments", zap.String("link_id", linkID), zap.Error(err))
		return
	}
	for _, a := range rows {
		if ctx.Err() != nil {
			return
		}
		p.settleOne(ctx, a)
	}
}

// backoff is when a failed attempt is tried again. Granting access backs off up to BackoffMax; taking it away is
// the priority path and never waits longer than RevokeBackoffMax. attempts counts the attempts made, this one
// included, so the first retry waits the base interval.
func (p *Provisioning) backoff(status string, attempts int) time.Time {
	limit := p.BackoffMax
	if status == AsgRevokePending && p.RevokeBackoffMax < limit {
		limit = p.RevokeBackoffMax
	}
	d := p.BackoffBase
	for i := 1; i < attempts && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	return p.now().Add(d)
}

// settleOne drives one queued assignment to its next state. Every remote call is idempotent (the correlation
// id is fixed per row), so a repeat after a crash or a lost answer asks the same question again.
func (p *Provisioning) settleOne(ctx context.Context, a Assignment) {
	switch a.Status {
	case AsgPlanned:
		p.requestOne(ctx, a)
	case AsgRevokePending:
		p.revokeOne(ctx, a)
	}
}

func (p *Provisioning) requestOne(ctx context.Context, a Assignment) {
	out, err := p.ac.RequestAssignment(ctx, AssignmentRequest{
		TenantID: a.TenantID, LegalEntityID: a.LegalEntityID, TargetPrincipalID: a.PrincipalID, RoleDefinitionID: a.RoleDefinitionID,
		Justification: fmt.Sprintf("federation mapping v%d for connection %s", a.MappingVersion, a.ConnectionID), CorrelationID: a.CorrelationID,
	})
	switch {
	case err == nil && out.Status == "PROVISIONED":
		p.record(ctx, a, AssignmentResult{Status: AsgProvisioned, RequestID: out.RequestID})
	case err == nil:
		// access control must never queue a federation request for approval; if it somehow did, it is not baseline access
		p.record(ctx, a, AssignmentResult{Status: AsgRefused, RequestID: out.RequestID, Reason: "approval_required"})
		p.raise(ctx, a, ExcPrivilegedRefused, "access control queued the request for approval, which a directory mapping may not do")
	case errors.Is(err, ErrPrivilegedRefused):
		p.record(ctx, a, AssignmentResult{Status: AsgRefused, Reason: "privileged_refused"})
		p.raise(ctx, a, ExcPrivilegedRefused, "role "+a.RoleDefinitionID+" is not baseline; it must be requested through governance approval")
	default:
		var rr *RequestRefused
		if errors.As(err, &rr) {
			p.record(ctx, a, AssignmentResult{Status: AsgRefused, Reason: trimReason(rr.Code, rr.Status)})
			p.raise(ctx, a, ExcRequestRefused, rr.Error())
			return
		}
		p.retry(ctx, a, AsgPlanned, err)
	}
}

func (p *Provisioning) revokeOne(ctx context.Context, a Assignment) {
	if a.RequestID == "" && a.Attempts == 0 {
		// no request was ever sent for it: there is nothing to revoke
		p.record(ctx, a, AssignmentResult{Status: AsgRevoked, Reason: "never_requested"})
		return
	}
	requestID := a.RequestID
	if requestID == "" {
		// a request may have been made whose answer was lost: replaying it returns the same request id
		out, err := p.ac.RequestAssignment(ctx, AssignmentRequest{
			TenantID: a.TenantID, LegalEntityID: a.LegalEntityID, TargetPrincipalID: a.PrincipalID, RoleDefinitionID: a.RoleDefinitionID,
			Justification: "federation mapping (resolving an unconfirmed request before revoking it)", CorrelationID: a.CorrelationID,
		})
		if err != nil {
			var rr *RequestRefused
			if errors.Is(err, ErrPrivilegedRefused) || errors.As(err, &rr) {
				p.record(ctx, a, AssignmentResult{Status: AsgRevoked, Reason: "never_granted"})
				return
			}
			p.retry(ctx, a, AsgRevokePending, err)
			return
		}
		requestID = out.RequestID
	}
	if err := p.ac.RevokeAssignment(ctx, a.TenantID, a.LegalEntityID, requestID, "fed-revoke-"+a.AssignmentID, "federation: access no longer entitled by the directory"); err != nil {
		var rr *RequestRefused
		if errors.As(err, &rr) {
			p.record(ctx, a, AssignmentResult{Status: AsgRevokePending, RequestID: requestID, Reason: trimReason(rr.Code, rr.Status), Error: rr.Error(), RetryAt: p.backoff(AsgRevokePending, a.Attempts+1)})
			p.raiseAt(ctx, a, ExcRequestRefused, "revoke refused: "+rr.Error(), a.Attempts+1)
			return
		}
		p.retry(ctx, a, AsgRevokePending, err)
		return
	}
	p.record(ctx, a, AssignmentResult{Status: AsgRevoked, RequestID: requestID})
	if _, err := p.repo.MarkAccessRevokedIfDone(ctx, a.TenantID, a.LinkID); err != nil {
		p.log.Warn("could not mark deprovisioning complete", zap.String("link_id", a.LinkID), zap.Error(err))
	}
}

func trimReason(code string, status int) string {
	if code != "" {
		return code
	}
	return fmt.Sprintf("http_%d", status)
}

func (p *Provisioning) record(ctx context.Context, a Assignment, res AssignmentResult) {
	if _, err := p.repo.RecordAssignmentResult(ctx, a.TenantID, a.AssignmentID, res); err != nil {
		p.log.Error("could not record an assignment result", zap.String("assignment_id", a.AssignmentID), zap.Error(err))
	}
}

func (p *Provisioning) retry(ctx context.Context, a Assignment, status string, cause error) {
	p.record(ctx, a, AssignmentResult{Status: status, Error: cause.Error(), Reason: "retrying", RetryAt: p.backoff(status, a.Attempts+1)})
	p.raiseAt(ctx, a, ExcAccessControlDown, "access control could not be reached or has not granted this service the right to act: "+trimTo(cause.Error(), 200), a.Attempts+1)
}

func trimTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (p *Provisioning) raise(ctx context.Context, a Assignment, kind, detail string) {
	p.raiseAt(ctx, a, kind, detail, p.ExceptionAfter)
}

// raiseAt records a sync exception for an assignment once its attempts reach the threshold (immediately for a
// final refusal).
func (p *Provisioning) raiseAt(ctx context.Context, a Assignment, kind, detail string, attempts int) {
	if attempts < p.ExceptionAfter {
		return
	}
	if err := p.repo.AddException(ctx, Exception{TenantID: a.TenantID, ConnectionID: a.ConnectionID, LinkID: a.LinkID,
		AssignmentID: a.AssignmentID, Kind: kind, Detail: detail}); err != nil {
		p.log.Warn("could not record a sync exception", zap.String("assignment_id", a.AssignmentID), zap.Error(err))
	}
}

// ── SCIM operations ──────────────────────────────────────────────────────────

func (p *Provisioning) core(t *SCIMTarget, r SCIMRequest, op, subject string, u *SCIMUser, rev *int) coreRequest {
	key, hash := p.dedupeKey(r)
	return coreRequest{Op: op, Source: SrcSCIM, LinkSource: SourceSCIM, Subject: subject, User: u, ExpectedRev: rev,
		DedupeKey: key, PayloadHash: hash, Corr: r.CorrelationID, Actor: "scim:" + t.Conn.ConnectionID}
}

// CreateUser handles POST /Users. An existing user with the same externalId is answered as itself (a directory's
// retry), never duplicated.
func (p *Provisioning) CreateUser(ctx context.Context, t *SCIMTarget, r SCIMRequest) (*Outcome, error) {
	u, err := ParseSCIMUser(r.Body)
	if err != nil {
		return nil, err
	}
	if !u.Active {
		return nil, scimBad("invalidValue", "a user cannot be created inactive")
	}
	return p.provision(ctx, t, p.core(t, r, OpCreate, u.ExternalID, u, nil))
}

// ReplaceUser handles PUT /Users/{id}.
func (p *Provisioning) ReplaceUser(ctx context.Context, t *SCIMTarget, id string, r SCIMRequest) (*Outcome, error) {
	u, err := ParseSCIMUser(r.Body)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		cur, gerr := p.repo.GetUser(ctx, t.TenantID, t.Conn.ConnectionID, id)
		if errors.Is(gerr, ErrUserNotFound) {
			return nil, rejection(http.StatusNotFound, "", "no such user")
		}
		if gerr != nil {
			return nil, gerr
		}
		if u.ExternalID != cur.Link.Subject {
			return nil, scimBad("mutability", "externalId is the directory's stable identifier and cannot change")
		}
		rev := cur.Link.AttributesRev
		out, perr := p.provision(ctx, t, p.core(t, r, opFor(u.Active), u.ExternalID, u, &rev))
		if errors.Is(perr, ErrStaleLink) {
			continue
		}
		return out, perr
	}
	return nil, rejection(http.StatusConflict, "", "the user was modified concurrently; retry")
}

func opFor(active bool) string {
	if active {
		return OpUpdate
	}
	return OpDeprovision
}

// PatchUser handles PATCH /Users/{id}. The operations are applied to a copy of the current record; one that
// cannot be applied changes nothing.
func (p *Provisioning) PatchUser(ctx context.Context, t *SCIMTarget, id string, r SCIMRequest) (*Outcome, error) {
	ops, err := ParsePatch(r.Body)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		cur, gerr := p.repo.GetUser(ctx, t.TenantID, t.Conn.ConnectionID, id)
		if errors.Is(gerr, ErrUserNotFound) {
			return nil, rejection(http.StatusNotFound, "", "no such user")
		}
		if gerr != nil {
			return nil, gerr
		}
		now := &SCIMUser{UserName: cur.Link.Username, ExternalID: cur.Link.Subject, DisplayName: cur.DisplayName, Email: cur.Email,
			Active: cur.Link.Status == LinkActive, Attrs: cur.Link.Attributes}
		if now.Attrs == nil {
			now.Attrs = Attributes{}
		}
		patched, perr := now.ApplyPatch(ops)
		if perr != nil {
			return nil, perr
		}
		rev := cur.Link.AttributesRev
		out, aerr := p.provision(ctx, t, p.core(t, r, opFor(patched.Active), patched.ExternalID, patched, &rev))
		if errors.Is(aerr, ErrStaleLink) {
			continue
		}
		return out, aerr
	}
	return nil, rejection(http.StatusConflict, "", "the user was modified concurrently; retry")
}

// DeleteUser handles DELETE /Users/{id}: for a directory, "remove" means this person must lose access now. The
// principal is disabled and its history kept; nothing is erased.
func (p *Provisioning) DeleteUser(ctx context.Context, t *SCIMTarget, id string, r SCIMRequest) (*Outcome, error) {
	cur, err := p.repo.GetUser(ctx, t.TenantID, t.Conn.ConnectionID, id)
	if errors.Is(err, ErrUserNotFound) {
		return nil, rejection(http.StatusNotFound, "", "no such user")
	}
	if err != nil {
		return nil, err
	}
	u := &SCIMUser{UserName: cur.Link.Username, ExternalID: cur.Link.Subject, DisplayName: cur.DisplayName, Email: cur.Email, Active: false, Attrs: cur.Link.Attributes}
	return p.provision(ctx, t, p.core(t, r, OpDeprovision, cur.Link.Subject, u, nil))
}

// GetUser handles GET /Users/{id}.
func (p *Provisioning) GetUser(ctx context.Context, t *SCIMTarget, id string) (*UserView, error) {
	v, err := p.repo.GetUser(ctx, t.TenantID, t.Conn.ConnectionID, id)
	if errors.Is(err, ErrUserNotFound) {
		return nil, rejection(http.StatusNotFound, "", "no such user")
	}
	return v, err
}

// ListUsers handles GET /Users with an optional filter.
func (p *Provisioning) ListUsers(ctx context.Context, t *SCIMTarget, filter string) ([]UserView, error) {
	if strings.TrimSpace(filter) == "" {
		return []UserView{}, nil // a directory enumerates by filter; this service does not offer the whole population
	}
	attr, value, err := ParseFilter(filter)
	if err != nil {
		return nil, err
	}
	out, err := p.repo.FindUsers(ctx, t.TenantID, t.Conn.ConnectionID, attr, value, 100)
	if out == nil {
		out = []UserView{}
	}
	return out, err
}

// ── just-in-time provisioning ────────────────────────────────────────────────

// JustInTime creates the principal, link and baseline assignments for the first verified login of a person a
// connection allows to be provisioned on arrival. It goes through exactly the pipeline SCIM does, so an email
// collision is quarantined and a mapping can only ever request baseline roles.
func (p *Provisioning) JustInTime(ctx context.Context, conn Connection, ver Version, v Verified, correlationID string) (*Link, error) {
	if v.Email == "" || !emailShape.MatchString(v.Email) {
		return nil, scimBad("invalidValue", "the token carries no usable email address")
	}
	active, _, err := p.repo.GetMappings(ctx, conn.TenantID, conn.ConnectionID)
	if err != nil {
		return nil, err
	}
	t := &SCIMTarget{TenantID: conn.TenantID, Conn: conn, Ver: ver, Mapping: active}
	u := &SCIMUser{UserName: v.Email, ExternalID: v.Subject, DisplayName: firstNonEmpty(v.DisplayName, v.Email), Email: v.Email, Active: true, Attrs: v.Attrs}
	if u.Attrs == nil {
		u.Attrs = Attributes{}
	}
	out, err := p.provision(ctx, t, coreRequest{Op: OpCreate, Source: SrcJIT, LinkSource: SourceJIT, Subject: v.Subject, User: u,
		DedupeKey: "jit:" + v.TokenHash, PayloadHash: v.TokenHash, Corr: correlationID, Actor: "jit:" + conn.ConnectionID})
	if err != nil {
		return nil, err
	}
	l := out.View.Link
	return &l, nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// ── reconciliation ───────────────────────────────────────────────────────────

// ReconcileRequest is a directory snapshot: the external ids of everyone the directory considers active.
type ReconcileRequest struct {
	Subjects []string `json:"subjects"`
	// Complete states that Subjects is the whole active population. Only a complete snapshot may deprovision;
	// an incomplete one reports and changes nothing.
	Complete bool `json:"complete"`
	// AllowMass lets a snapshot deprovision more than the safety share of the population in one pass.
	AllowMass bool `json:"allow_mass"`
}

// ReconcileResult reports what a reconciliation found and did.
type ReconcileResult struct {
	EventID          string   `json:"event_id"`
	Checked          int      `json:"checked"`
	Deprovisioned    int      `json:"deprovisioned"`
	WouldDeprovision int      `json:"would_deprovision"`
	Failed           int      `json:"failed"`
	UnknownSubjects  []string `json:"unknown_subjects,omitempty"`
	UnknownCount     int      `json:"unknown_count"`
	Applied          bool     `json:"applied"`
}

// Reconcile is ReconcileDirectorySync: the safety net for a deprovisioning the directory never sent. People
// the platform holds as active that the directory no longer lists are deprovisioned through the normal
// pipeline. An empty or implausibly destructive snapshot is refused, because a broken export must not be able to
// switch off a whole tenant.
func (p *Provisioning) Reconcile(ctx context.Context, tenantID, actor, correlationID, connectionID string, req ReconcileRequest) (*ReconcileResult, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	if len(req.Subjects) > 100000 {
		return nil, fmt.Errorf("%w: the snapshot is too large", ErrInvalid)
	}
	d, err := p.conns.GetConnection(ctx, tenantID, connectionID)
	if err != nil {
		return nil, err
	}
	ver := activeVersion(d)
	if ver == nil || d.Status == ConnRevoked {
		return nil, fmt.Errorf("%w: the connection is not in service", ErrTransition)
	}
	listed := map[string]bool{}
	for _, s := range req.Subjects {
		if !validSubject(s) {
			return nil, fmt.Errorf("%w: the snapshot holds a malformed subject", ErrInvalid)
		}
		listed[s] = true
	}
	links, err := p.repo.ActiveLinks(ctx, tenantID, connectionID)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	var missing []Link
	for _, l := range links {
		known[l.Subject] = true
		if !listed[l.Subject] {
			missing = append(missing, l)
		}
	}
	out := &ReconcileResult{Checked: len(links), WouldDeprovision: len(missing)}
	for s := range listed {
		if !known[s] {
			out.UnknownCount++
			if len(out.UnknownSubjects) < 100 {
				out.UnknownSubjects = append(out.UnknownSubjects, s)
			}
		}
	}
	if req.Complete {
		if len(listed) == 0 && len(links) > 0 {
			return nil, fmt.Errorf("%w: an empty snapshot would deprovision everyone; it is refused", ErrInvalid)
		}
		limit := int(float64(len(links)) * p.ReconcileMaxShare)
		if limit < 10 {
			limit = 10
		}
		if len(missing) > limit && !req.AllowMass {
			return nil, fmt.Errorf("%w: the snapshot would deprovision %d of %d people (more than the safety limit of %d); confirm with allow_mass if that is intended",
				ErrConflict, len(missing), len(links), limit)
		}
	}

	key := "reconcile:" + correlationID
	if correlationID == "" {
		key = fmt.Sprintf("reconcile:%d", p.now().UnixNano())
	}
	summary := hex.EncodeToString(sha256Sum(fmt.Sprint(len(req.Subjects), req.Complete, len(missing))))
	ev, created, err := p.repo.BeginEvent(ctx, ProvEvent{TenantID: tenantID, ConnectionID: connectionID, DedupeKey: key,
		Source: SrcReconcile, Operation: OpReconcile, PayloadHash: summary})
	if err != nil {
		return nil, err
	}
	out.EventID = ev.EventID
	if !created && ev.Status == ProvReconciled {
		return nil, fmt.Errorf("%w: this reconciliation was already run", ErrConflict)
	}

	if req.Complete {
		for _, l := range missing {
			u := &SCIMUser{ExternalID: l.Subject, Active: false, Attrs: l.Attributes, UserName: l.Username}
			_, perr := p.provision(ctx, &SCIMTarget{TenantID: tenantID, Conn: d.Connection, Ver: *ver, Mapping: nil}, coreRequest{
				Op: OpDeprovision, Source: SrcReconcile, LinkSource: SourceSCIM, Subject: l.Subject, User: u,
				DedupeKey: "reconcile:" + ev.EventID + ":" + l.Subject, PayloadHash: summary, Corr: correlationID, Actor: "reconcile:" + actor})
			if perr != nil {
				out.Failed++
				p.log.Warn("reconcile could not deprovision a person", zap.String("link_id", l.LinkID), zap.Error(perr))
				continue
			}
			out.Deprovisioned++
		}
		out.Applied = true
	}
	result := map[string]any{"checked": out.Checked, "deprovisioned": out.Deprovisioned, "would_deprovision": out.WouldDeprovision,
		"failed": out.Failed, "unknown": out.UnknownCount, "complete": req.Complete}
	if derr := p.repo.DecideEvent(ctx, tenantID, ev.EventID, ProvReconciled, "", "", result); derr != nil {
		p.log.Error("could not record a reconciliation", zap.String("event_id", ev.EventID), zap.Error(derr))
	}
	return out, nil
}

// ── the worker ───────────────────────────────────────────────────────────────

// SweepResult counts what one sweep did.
type SweepResult struct {
	Assignments int
	Evictions   int
	Escalations int
}

// Sweep is one pass of the worker: due assignment work (revocations first), sessions still to end, and
// deprovisionings that have outlived their objective.
func (p *Provisioning) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	due, err := p.repo.ClaimDueAssignments(ctx, p.SweepBatch, p.Lease)
	if err != nil {
		return res, fmt.Errorf("claim assignments: %w", err)
	}
	touched := map[[2]string]bool{}
	for _, a := range due {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		p.settleOne(ctx, a)
		res.Assignments++
		touched[[2]string{a.TenantID, a.LinkID}] = true
	}
	for k := range touched {
		if _, err := p.repo.MarkAccessRevokedIfDone(ctx, k[0], k[1]); err != nil {
			p.log.Warn("could not mark deprovisioning complete", zap.String("link_id", k[1]), zap.Error(err))
		}
	}
	pending, err := p.repo.LinksAwaitingEviction(ctx, p.SweepBatch)
	if err != nil {
		return res, fmt.Errorf("list evictions: %w", err)
	}
	for _, l := range pending {
		if p.evict(ctx, l.TenantID, l.PrincipalID, l.LinkID) {
			res.Evictions++
		}
	}
	late, err := p.repo.LateDeprovisions(ctx, p.now(), p.SweepBatch)
	if err != nil {
		return res, fmt.Errorf("list late deprovisions: %w", err)
	}
	for _, l := range late {
		detail := "deprovisioning has not finished within its objective"
		if l.SessionsEvictedAt == nil {
			detail += "; the person's sessions are still live"
		}
		if ok, err := p.repo.EscalateDeprovision(ctx, l.TenantID, l.LinkID, detail); err != nil {
			p.log.Warn("could not escalate a late deprovisioning", zap.String("link_id", l.LinkID), zap.Error(err))
		} else if ok {
			res.Escalations++
			p.log.Error("deprovisioning missed its objective", zap.String("link_id", l.LinkID), zap.String("tenant_id", l.TenantID))
		}
	}
	return res, nil
}

// Run sweeps until ctx ends.
func (p *Provisioning) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		p.log.Warn("federation worker disabled: no sweep interval")
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sctx, cancel := context.WithTimeout(ctx, interval*4)
			res, err := p.Sweep(sctx)
			cancel()
			if err != nil {
				p.log.Error("federation sweep failed", zap.Error(err))
			} else if res.Assignments+res.Evictions+res.Escalations > 0 {
				p.log.Info("federation sweep", zap.Int("assignments", res.Assignments), zap.Int("evictions", res.Evictions), zap.Int("escalations", res.Escalations))
			}
		}
	}
}
