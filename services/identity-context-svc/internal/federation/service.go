package federation

import (
	"context"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"
)

// Service is the governed administration of federation: the named commands of spec 4.10 other than
// directory sync (see provision.go). Authorization of the caller is the handler's job; the rules that
// make a command safe whoever calls it (validation, maker-checker, fingerprint) live here and in the store.
type Service struct {
	repo   Repo
	policy URLPolicy
	keys   *KeyManager
	log    *zap.Logger
}

// NewService builds the service. policy checks JWKS locations against the egress rules; keys refreshes
// signing keys on demand.
func NewService(repo Repo, policy URLPolicy, keys *KeyManager, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{repo: repo, policy: policy, keys: keys, log: log}
}

// ConfigureRequest proposes a connection (no ConnectionID) or a new version of one (with it).
type ConfigureRequest struct {
	ConnectionID string `json:"connection_id,omitempty"`
	Name         string `json:"name,omitempty"`
	Config       Config `json:"config"`
}

func requireCommand(tenantID, actor string) error {
	if tenantID == "" || actor == "" {
		return fmt.Errorf("%w: tenant and actor are required", ErrInvalid)
	}
	return nil
}

// Configure is ConfigureFederation. The result always awaits approval: a connection cannot authenticate
// anyone on its author's word alone.
func (s *Service) Configure(ctx context.Context, tenantID, actor, correlationID string, req ConfigureRequest) (*ConnectionDetail, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	cfg := req.Config.Normalize()
	if err := cfg.Validate(s.policy); err != nil {
		return nil, err
	}
	v := Version{Config: cfg, Fingerprint: cfg.Fingerprint(), CreatedBy: actor, TenantID: tenantID}
	if req.ConnectionID == "" {
		name := strings.TrimSpace(req.Name)
		if name == "" || len(name) > 200 {
			return nil, fmt.Errorf("%w: name is required (at most 200 characters)", ErrInvalid)
		}
		id := ulid.Make().String()
		v.ConnectionID = id
		c := Connection{ConnectionID: id, TenantID: tenantID, Name: name, CreatedBy: actor}
		if err := s.repo.CreateConnection(ctx, c, v, actor, correlationID); err != nil {
			return nil, err
		}
		return s.repo.GetConnection(ctx, tenantID, id)
	}
	if _, err := s.repo.AddVersion(ctx, tenantID, req.ConnectionID, v, actor, correlationID); err != nil {
		return nil, err
	}
	return s.repo.GetConnection(ctx, tenantID, req.ConnectionID)
}

// Approve is ApproveFederation: the approver names the version and the fingerprint they were shown.
func (s *Service) Approve(ctx context.Context, tenantID, actor, correlationID, connectionID string, version int, fingerprint, reason string) (*ConnectionDetail, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	return s.repo.DecideVersion(ctx, tenantID, connectionID, version, true, actor, fingerprint, strings.TrimSpace(reason), correlationID)
}

// Reject declines a pending version. A reason is required.
func (s *Service) Reject(ctx context.Context, tenantID, actor, correlationID, connectionID string, version int, reason string) (*ConnectionDetail, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a reason is required to reject", ErrInvalid)
	}
	return s.repo.DecideVersion(ctx, tenantID, connectionID, version, false, actor, "", strings.TrimSpace(reason), correlationID)
}

// RotateMetadata is RotateMetadataOrCertificateRef: it proposes a new version identical to the approved
// one except for the JWKS location. Where the signing keys come from is a trust decision, so it is
// approved like any other change. Rotation of the KEYS at an unchanged location needs no approval: that
// is what a refresh does.
func (s *Service) RotateMetadata(ctx context.Context, tenantID, actor, correlationID, connectionID, jwksURI string) (*ConnectionDetail, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	d, err := s.repo.GetConnection(ctx, tenantID, connectionID)
	if err != nil {
		return nil, err
	}
	cur := activeVersion(d)
	if cur == nil {
		return nil, fmt.Errorf("%w: there is no approved version to rotate", ErrConflict)
	}
	cfg := cur.Config
	cfg.JWKSURI = strings.TrimSpace(jwksURI)
	if cfg.JWKSURI == cur.Config.JWKSURI {
		return nil, fmt.Errorf("%w: the metadata location is unchanged", ErrInvalid)
	}
	return s.Configure(ctx, tenantID, actor, correlationID, ConfigureRequest{ConnectionID: connectionID, Config: cfg})
}

// RefreshKeys fetches the signing keys now from the approved location.
func (s *Service) RefreshKeys(ctx context.Context, tenantID, connectionID string) (int, error) {
	d, err := s.repo.GetConnection(ctx, tenantID, connectionID)
	if err != nil {
		return 0, err
	}
	cur := activeVersion(d)
	if cur == nil || (d.Status != ConnActive && d.Status != ConnDegraded) {
		return 0, fmt.Errorf("%w: the connection is not in service", ErrTransition)
	}
	return s.keys.Refresh(ctx, d.Connection, cur.Config)
}

// Suspend stops new federated logins. Existing bearer tokens expire on their own short lifetime.
func (s *Service) Suspend(ctx context.Context, tenantID, actor, correlationID, connectionID, reason string) (*Connection, error) {
	return s.transition(ctx, tenantID, actor, correlationID, connectionID, ConnSuspended, reason, true)
}

// Resume returns a suspended connection to service.
func (s *Service) Resume(ctx context.Context, tenantID, actor, correlationID, connectionID, reason string) (*Connection, error) {
	return s.transition(ctx, tenantID, actor, correlationID, connectionID, ConnActive, reason, false)
}

// Revoke ends a connection permanently.
func (s *Service) Revoke(ctx context.Context, tenantID, actor, correlationID, connectionID, reason string) (*Connection, error) {
	return s.transition(ctx, tenantID, actor, correlationID, connectionID, ConnRevoked, reason, true)
}

func (s *Service) transition(ctx context.Context, tenantID, actor, correlationID, connectionID, to, reason string, needReason bool) (*Connection, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if needReason && reason == "" {
		return nil, fmt.Errorf("%w: a reason is required", ErrInvalid)
	}
	return s.repo.SetStatus(ctx, tenantID, connectionID, to, actor, reason, correlationID)
}

// Get is GetFederationConfig.
func (s *Service) Get(ctx context.Context, tenantID, connectionID string) (*ConnectionDetail, error) {
	return s.repo.GetConnection(ctx, tenantID, connectionID)
}

// List returns a tenant's connections.
func (s *Service) List(ctx context.Context, tenantID string) ([]Connection, error) {
	return s.repo.ListConnections(ctx, tenantID)
}

// Logins returns the evidence of recent federated logins on a connection.
func (s *Service) Logins(ctx context.Context, tenantID, connectionID string, limit int) ([]LoginRecord, error) {
	if _, err := s.repo.GetConnection(ctx, tenantID, connectionID); err != nil {
		return nil, err
	}
	return s.repo.ListLogins(ctx, tenantID, connectionID, limit)
}

// LinkRequest links an external subject to an existing principal by administrator's decision.
type LinkRequest struct {
	ConnectionID string `json:"connection_id"`
	Subject      string `json:"subject"`
	PrincipalID  string `json:"principal_id"`
}

// Link is the administrator's deliberate binding of one external subject to one principal. The email
// address never appears in it: the subject is the issuer's stable identifier.
func (s *Service) Link(ctx context.Context, tenantID, actor, correlationID string, req LinkRequest) (*Link, error) {
	if err := requireCommand(tenantID, actor); err != nil {
		return nil, err
	}
	if req.ConnectionID == "" || req.PrincipalID == "" || !validSubject(req.Subject) {
		return nil, fmt.Errorf("%w: connection_id, principal_id and a valid subject are required", ErrInvalid)
	}
	return s.repo.CreateLink(ctx, Link{TenantID: tenantID, ConnectionID: req.ConnectionID, Subject: req.Subject,
		PrincipalID: req.PrincipalID, Source: SourceAdmin, LinkedBy: actor}, actor, correlationID)
}

// GetLink is GetExternalSubjectLink.
func (s *Service) GetLink(ctx context.Context, tenantID, linkID string) (*Link, error) {
	return s.repo.GetLink(ctx, tenantID, linkID)
}

// ListLinks lists links, optionally for one principal or issuer.
func (s *Service) ListLinks(ctx context.Context, tenantID, principalID, issuer string, limit int) ([]Link, error) {
	return s.repo.ListLinks(ctx, tenantID, principalID, issuer, limit)
}

func activeVersion(d *ConnectionDetail) *Version {
	if d.ActiveVersion == nil {
		return nil
	}
	for i := range d.Versions {
		if d.Versions[i].Version == *d.ActiveVersion {
			return &d.Versions[i]
		}
	}
	return nil
}
