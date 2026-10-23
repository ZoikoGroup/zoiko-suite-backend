package xicconn

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/vault"
	"zoiko.io/eventing/egress"
)

// Contracts is the read side of the registry.
type Contracts interface {
	GetVersion(ctx context.Context, id string) (*registry.ContractVersion, error)
}

// Config holds the policy knobs ZS-SVC-T-001 §19.1 leaves open, each with a safe default.
type Config struct {
	AuthSessionTTL   time.Duration // lifetime of a provider authorization state
	ExpiryWarning    time.Duration // how long before expiry a credential is EXPIRING
	MaxGrace         time.Duration // longest the previous credential may overlap a rotation
	RotationInterval time.Duration // how long a credential may live before rotation is due
	BreakGlassMax    time.Duration // longest a break-glass approval stays usable
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{AuthSessionTTL: 10 * time.Minute, ExpiryWarning: 14 * 24 * time.Hour, MaxGrace: 7 * 24 * time.Hour,
		RotationInterval: 90 * 24 * time.Hour, BreakGlassMax: 4 * time.Hour}
}

// Service runs the connection commands. Anything that talks to the outside world (the registry, the vault, DNS)
// is done on a snapshot BEFORE the transaction opens; the transaction re-checks that the connection is still the
// version that was checked. No network call runs while a row is locked.
type Service struct {
	Repo      Repo
	Contracts Contracts
	Vault     vault.Verifier
	Egress    egress.Policy
	// AllowedRegions are the regions this deployment may send data to; EMPTY MEANS NONE.
	AllowedRegions []string
	// RedirectAllowlist is the set of pre-registered OAuth redirect targets, exact match.
	RedirectAllowlist []string
	// Stage, when set, applies the staged production rollout (Wave 8) to every operation of a production
	// connection. A nil gate leaves production connections ungated.
	Stage StageGate
	Cfg   Config
	Clock func() time.Time
}

// StageGate decides whether a production connection's staged rollout admits an operation now. It returns nil to
// admit, a *Refusal when the stage refuses, and any other error when the stage cannot be read (the caller fails
// closed).
type StageGate interface {
	Admit(ctx context.Context, c *Connection, operationID string, now time.Time) error
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) cfg() Config {
	if s.Cfg == (Config{}) {
		return DefaultConfig()
	}
	return s.Cfg
}

func tenantOf(ctx context.Context) string { return middleware.GetTenantID(ctx) }

func (s *Service) caller(ctx context.Context, a Actor) vault.Caller {
	return vault.Caller{PrincipalID: a.PrincipalID, TenantID: tenantOf(ctx), CorrelationID: a.CorrelationID}
}

func out(a Actor, typ string, c *Connection, extra map[string]any) OutEvent {
	d := map[string]any{"tenant_id": c.TenantID, "connection_id": c.ConnectionID, "connector_id": c.ConnectorID,
		"contract_version_id": c.ContractVersionID}
	for k, v := range extra {
		d[k] = v
	}
	return OutEvent{Type: typ, AggregateType: "connection", AggregateID: c.ConnectionID, ActorID: a.PrincipalID, CorrelationID: a.CorrelationID, Data: d}
}

func record(tx *Tx, a Actor, kind, from, to, reason string, at time.Time) {
	tx.NewEvent = &Event{Kind: kind, From: from, To: to, Actor: a.PrincipalID, Reason: reason, At: at}
}

func bad(path, rule string) error {
	return &Validation{Issues: []registry.Issue{{Path: path, Rule: rule}}}
}

func lowerSet(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func difference(a, b []string) []string {
	have := map[string]bool{}
	for _, s := range b {
		have[s] = true
	}
	var out []string
	for _, s := range a {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// pinned loads the contract version a connection is pinned to.
func (s *Service) pinned(ctx context.Context, id string) (*registry.ContractVersion, error) {
	v, err := s.Contracts.GetVersion(ctx, id)
	if err != nil {
		if registry.IsNotFound(err) {
			return nil, ErrContractUnavailable
		}
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	return v, nil
}

// ── create ───────────────────────────────────────────────────────────────────

// CreateInput is a create request.
type CreateInput struct {
	Environment        string            `json:"environment"`
	ConnectorID        string            `json:"connector_id"`
	ContractVersionID  string            `json:"contract_version_id"`
	Region             string            `json:"region"`
	Endpoints          map[string]string `json:"endpoints"`
	ProviderAccountRef string            `json:"provider_account_ref"`
	ExternalOrgRef     string            `json:"external_org_ref,omitempty"`
	BridgeID           string            `json:"bridge_id,omitempty"`
}

// Create makes a DRAFT connection pinned to a PUBLISHED contract version. The provider account reference is
// stored only as a salted hash; the raw value is dropped here.
func (s *Service) Create(ctx context.Context, a Actor, in CreateInput, idemKey string) (*Connection, bool, error) {
	tenant := tenantOf(ctx)
	var is []registry.Issue
	add := func(p, r string) { is = append(is, registry.Issue{Path: p, Rule: r}) }
	in.Environment = strings.ToUpper(strings.TrimSpace(in.Environment))
	if in.Environment != EnvProduction && in.Environment != EnvStaging && in.Environment != EnvSandbox {
		add("environment", "PRODUCTION, STAGING or SANDBOX")
	}
	in.Region = strings.ToLower(strings.TrimSpace(in.Region))
	if in.Region == "" {
		add("region", "required")
	}
	if ref := strings.TrimSpace(in.ProviderAccountRef); ref == "" || len(ref) > 256 {
		add("provider_account_ref", "required, at most 256 characters")
	}
	if len(is) > 0 {
		return nil, false, &Validation{is}
	}
	v, err := s.pinned(ctx, in.ContractVersionID)
	if err != nil {
		return nil, false, err
	}
	if v.ConnectorID != in.ConnectorID {
		add("contract_version_id", "does not belong to this connector")
	}
	if v.Status != registry.VersionPublished {
		add("contract_version_id", "a new connection pins a PUBLISHED contract version")
	}
	// the destination comes from the contract's endpoint profiles, never from free text (I-18)
	eps := v.UsedEndpointProfiles()
	for name, info := range eps {
		host, ok := in.Endpoints[name]
		switch {
		case !ok:
			add("endpoints."+name, "an approved host of this endpoint profile is required")
		case !containsFold(info.Hosts, host):
			add("endpoints."+name, "not an approved host of the endpoint profile")
		}
		if !containsFold(info.Regions, in.Region) {
			add("region", "the endpoint profile "+name+" does not serve this region")
		}
	}
	for name := range in.Endpoints {
		if _, ok := eps[name]; !ok {
			add("endpoints."+name, "the contract has no such endpoint profile")
		}
	}
	if len(is) > 0 {
		return nil, false, &Validation{is}
	}
	if !containsFold(s.AllowedRegions, in.Region) {
		return nil, false, ErrRegionDenied
	}
	endpoints := map[string]string{}
	for k, h := range in.Endpoints {
		endpoints[k] = strings.ToLower(h)
	}
	now := s.now()
	c := &Connection{ConnectionID: uuid.NewString(), TenantID: tenant, Environment: in.Environment, ConnectorID: in.ConnectorID,
		ContractVersionID: in.ContractVersionID, Status: StatusDraft, Region: in.Region, Endpoints: endpoints,
		ProviderAccountHash: HashAccountRef(tenant, in.ConnectorID, in.ProviderAccountRef), BridgeID: in.BridgeID,
		Bindings: []Binding{}, CreatedBy: a.PrincipalID, Version: 1, CreatedAt: now, UpdatedAt: now}
	if strings.TrimSpace(in.ExternalOrgRef) != "" {
		c.ExternalOrgHash = HashAccountRef(tenant, in.ConnectorID, "org:"+in.ExternalOrgRef)
	}
	c.ConfigFingerprint = ConfigFingerprint(c)
	reqHash := sha("create", in.Environment, in.ConnectorID, in.ContractVersionID, in.Region, c.ProviderAccountHash, mustJSON(endpoints))
	created, err := s.Repo.Create(ctx, c, idemKey, reqHash, out(a, "integration.connection.created", c,
		map[string]any{"environment": c.Environment, "region_profile": c.Region}))
	if err != nil {
		return nil, false, err
	}
	return c, created, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ── authorize and bind ───────────────────────────────────────────────────────

// AuthorizeInput starts authorization.
type AuthorizeInput struct {
	Mode        string `json:"mode"` // PRE_PROVISIONED | OAUTH_AUTH_CODE
	RedirectURI string `json:"redirect_uri,omitempty"`
}

// AuthorizeResult carries the one-time state of an OAuth session. It is shown once and only its hash is kept.
type AuthorizeResult struct {
	Connection  *Connection `json:"connection"`
	SessionID   string      `json:"session_id,omitempty"`
	State       string      `json:"state,omitempty"`
	ExpiresAt   *time.Time  `json:"expires_at,omitempty"`
	RedirectURI string      `json:"redirect_uri,omitempty"`
}

func (s *Service) redirectRegistered(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, r := range s.RedirectAllowlist {
		if r == uri { // exact: a prefix or pattern match is how open redirects happen
			return true
		}
	}
	return false
}

// Authorize starts provider authorization (or declares a pre-provisioned credential) and moves DRAFT or
// SUSPENDED to AUTHORIZING.
func (s *Service) Authorize(ctx context.Context, a Actor, id string, expected int64, in AuthorizeInput) (*AuthorizeResult, error) {
	in.Mode = strings.ToUpper(strings.TrimSpace(in.Mode))
	if in.Mode != "PRE_PROVISIONED" && in.Mode != "OAUTH_AUTH_CODE" {
		return nil, bad("mode", "PRE_PROVISIONED or OAUTH_AUTH_CODE")
	}
	snap, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v, err := s.pinned(ctx, snap.ContractVersionID)
	if err != nil {
		return nil, err
	}
	if in.Mode == "OAUTH_AUTH_CODE" {
		hasOAuth := false
		for _, p := range v.UsedAuthProfiles() {
			hasOAuth = hasOAuth || p.Mechanism == "OAUTH2_AUTH_CODE"
		}
		if !hasOAuth {
			return nil, bad("mode", "the pinned contract has no OAuth authorization-code profile")
		}
		if !s.redirectRegistered(in.RedirectURI) {
			return nil, ErrRedirectNotRegistered
		}
	} else if in.RedirectURI != "" {
		return nil, bad("redirect_uri", "only with OAUTH_AUTH_CODE")
	}
	res := &AuthorizeResult{}
	c, err := s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if tx.C.Version != expected {
			return ErrVersionConflict
		}
		if tx.C.Status != StatusDraft && tx.C.Status != StatusSuspended {
			return ErrTransition
		}
		now := s.now()
		from := tx.C.Status
		tx.C.Status, tx.C.StatusReason = StatusAuthorizing, ""
		record(tx, a, "AUTHORIZE", from, StatusAuthorizing, in.Mode, now)
		if in.Mode == "OAUTH_AUTH_CODE" {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			state := base64.RawURLEncoding.EncodeToString(raw)
			exp := now.Add(s.cfg().AuthSessionTTL)
			tx.NewSession = &Session{SessionID: uuid.NewString(), StateHash: sha("state", state), RedirectURI: in.RedirectURI,
				ExpiresAt: exp, CreatedBy: a.PrincipalID, CreatedAt: now}
			res.SessionID, res.State, res.ExpiresAt, res.RedirectURI = tx.NewSession.SessionID, state, &exp, in.RedirectURI
		}
		tx.Out = append(tx.Out, out(a, "integration.connection.authorizing", tx.C, map[string]any{"mode": in.Mode}))
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Connection = c
	return res, nil
}

// BindInput binds a credential reference to one authentication profile.
type BindInput struct {
	AuthProfile   string     `json:"auth_profile"`
	CredentialRef string     `json:"credential_ref"`
	GrantedScopes []string   `json:"granted_scopes"`
	ConsentActor  string     `json:"consent_actor"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	// State is the one-time authorization state; required for an OAuth authorization-code profile.
	State string `json:"state,omitempty"`
	// AcceptExcessScopes records that someone reviewed scopes beyond what the contract requires.
	AcceptExcessScopes bool `json:"accept_excess_scopes,omitempty"`
}

func (s *Service) mapVault(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, vault.ErrUnavailable):
		return fmt.Errorf("%w: secret vault", ErrDependency)
	default:
		return fmt.Errorf("%w: %v", ErrCredentialInvalid, err)
	}
}

// Bind records a credential reference for an authentication profile. The vault confirms the reference exists,
// belongs to this tenant and has an active version; the secret itself is never read.
func (s *Service) Bind(ctx context.Context, a Actor, id string, expected int64, in BindInput) (*Connection, error) {
	tenant := tenantOf(ctx)
	if !domain.ValidCredentialRef(in.CredentialRef) {
		return nil, bad("credential_ref", "must be a vault secret policy id")
	}
	if strings.TrimSpace(in.ConsentActor) == "" || len(in.ConsentActor) > 255 {
		return nil, bad("consent_actor", "who gave the consent or administered the credential is required")
	}
	snap, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if snap.Version != expected {
		return nil, ErrVersionConflict
	}
	v, err := s.pinned(ctx, snap.ContractVersionID)
	if err != nil {
		return nil, err
	}
	info, ok := v.UsedAuthProfiles()[in.AuthProfile]
	if !ok {
		return nil, bad("auth_profile", "the pinned contract has no such authentication profile")
	}
	now := s.now()
	granted := lowerSet(in.GrantedScopes)
	required := lowerSet(info.ScopesRequired)
	if missing := difference(required, granted); len(missing) > 0 {
		return nil, fmt.Errorf("%w: %d required scope(s) are not granted", ErrScopesInsufficient, len(missing))
	}
	excess := difference(granted, required)
	if len(excess) > 0 && !in.AcceptExcessScopes {
		return nil, fmt.Errorf("%w: %d scope(s) beyond the contract's need", ErrExcessScopes, len(excess))
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		return nil, ErrCredentialExpired
	}
	if info.Mechanism == "MTLS" && in.ExpiresAt == nil {
		return nil, bad("expires_at", "an mTLS binding carries the certificate's expiry")
	}
	if err := s.mapVault(s.Vault.Verify(ctx, in.CredentialRef, s.caller(ctx, a))); err != nil {
		return nil, err
	}

	c, err := s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if tx.C.Version != expected {
			return ErrVersionConflict
		}
		if tx.C.Status != StatusAuthorizing {
			return ErrTransition
		}
		if info.Mechanism == "OAUTH2_AUTH_CODE" {
			sess := tx.Session
			if sess == nil || sess.ConsumedAt != nil || !now.Before(sess.ExpiresAt) ||
				subtle.ConstantTimeCompare([]byte(sess.StateHash), []byte(sha("state", in.State))) != 1 {
				return ErrAuthState
			}
			tx.ConsumeSession = true
		} else if in.State != "" {
			return bad("state", "only for an OAuth authorization-code profile")
		}
		b := Binding{BindingID: uuid.NewString(), AuthProfile: in.AuthProfile, CurrentRef: strings.ToLower(in.CredentialRef),
			GrantedScopes: granted, ScopeFingerprint: ScopeFingerprint(granted), Status: BindingOK, ExpiresAt: in.ExpiresAt,
			BoundBy: a.PrincipalID, BoundAt: now}
		if len(excess) > 0 {
			b.ExcessScopeAckBy = a.PrincipalID
		}
		due := now.Add(s.cfg().RotationInterval)
		if in.ExpiresAt != nil && in.ExpiresAt.Before(due) {
			due = *in.ExpiresAt
		}
		b.RotationDueAt = &due
		replaced := false
		for i := range tx.C.Bindings {
			if tx.C.Bindings[i].AuthProfile == in.AuthProfile {
				b.BindingID = tx.C.Bindings[i].BindingID
				tx.C.Bindings[i] = b
				replaced = true
			}
		}
		if !replaced {
			tx.C.Bindings = append(tx.C.Bindings, b)
		}
		tx.C.ConsentActor = in.ConsentActor
		tx.C.ConfigFingerprint = ConfigFingerprint(tx.C)
		record(tx, a, "BIND", StatusAuthorizing, StatusAuthorizing, in.AuthProfile, now)
		tx.Out = append(tx.Out, out(a, "integration.connection.credential-bound", tx.C,
			map[string]any{"auth_profile": in.AuthProfile, "scope_fingerprint": b.ScopeFingerprint, "excess_scopes": len(excess)}))
		return nil
	})
	if errors.Is(err, ErrCrossTenantCredential) {
		// a credential reference seen in a second place is a defect and a security signal, raised even though
		// the bind itself is refused
		_ = s.Repo.RecordSignal(ctx, tenant, OutEvent{Type: "integration.security.cross-tenant-credential", AggregateType: "connection",
			AggregateID: id, ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
			Data: map[string]any{"tenant_id": tenant, "connection_id": id, "auth_profile": in.AuthProfile, "reason_code": "XIC-008"}})
	}
	return c, err
}

// ── validate ─────────────────────────────────────────────────────────────────

// ValidateResult is a validation run and the connection after it.
type ValidateResult struct {
	Connection *Connection `json:"connection"`
	Check      *Check      `json:"check"`
}

type capability struct {
	Operation string `json:"operation_id"`
	Direction string `json:"direction"`
	Ready     bool   `json:"ready"`
	Blocker   string `json:"blocker,omitempty"`
}

// Validate runs the non-destructive validation of §7.3: the pinned contract is still usable, every
// authentication profile has an unexpired, vault-confirmed binding covering the scopes the contract requires, the
// region and every destination host pass egress policy. It records the run whether it passes or not. The run key
// makes a repeat of the same validation return the same run.
func (s *Service) Validate(ctx context.Context, a Actor, id string, expected int64, runKey string) (*ValidateResult, error) {
	snap, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if snap.Version != expected {
		return nil, ErrVersionConflict
	}
	switch snap.Status {
	case StatusAuthorizing, StatusValidating, StatusActive, StatusDegraded:
	default:
		return nil, ErrTransition
	}
	checkID := uuid.NewString()
	if runKey != "" {
		checkID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(id+"|"+runKey)).String()
	}
	v, err := s.pinned(ctx, snap.ContractVersionID)
	var issues []registry.Issue
	var caps []capability
	if err != nil {
		if !errors.Is(err, ErrContractUnavailable) {
			return nil, err
		}
		issues = append(issues, registry.Issue{Path: "contract_version_id", Rule: "the pinned contract version is not available"})
	} else {
		issues, caps, err = s.validateAgainst(ctx, a, snap, v)
		if err != nil {
			return nil, err // a dependency outage is not a failed validation; nothing is recorded
		}
	}
	res := &ValidateResult{}
	c, err := s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if tx.C.Version != expected {
			return ErrVersionConflict
		}
		for i := range tx.Checks {
			if tx.Checks[i].CheckID == checkID { // the same run, replayed
				ck := tx.Checks[i]
				res.Check = &ck
				return errReplay
			}
		}
		now := s.now()
		fp := ConfigFingerprint(tx.C)
		ck := &Check{CheckID: checkID, Kind: CheckValidation, Outcome: Passed, ConfigFingerprint: fp, Issues: issues, RunBy: a.PrincipalID, At: now}
		if len(issues) > 0 {
			ck.Outcome = Failed
		}
		capsJSON, _ := json.Marshal(caps)
		ck.Detail = capsJSON
		tx.NewCheck, res.Check = ck, ck
		from := tx.C.Status
		switch {
		case ck.Outcome == Passed:
			tx.C.ConfigFingerprint, tx.C.CapabilitySnapshot = fp, capsJSON
			switch from {
			case StatusAuthorizing:
				tx.C.Status = StatusValidating
			case StatusDegraded:
				tx.C.Status, tx.C.StatusReason = StatusActive, ""
			}
		case from == StatusActive:
			tx.C.Status, tx.C.StatusReason = StatusDegraded, "VALIDATION_FAILED"
		}
		record(tx, a, "VALIDATE", from, tx.C.Status, ck.Outcome, now)
		tx.Out = append(tx.Out, out(a, "integration.connection."+map[string]string{Passed: "validated", Failed: "validation-failed"}[ck.Outcome], tx.C,
			map[string]any{"check_id": checkID, "issue_count": len(issues)}))
		return nil
	})
	if errors.Is(err, errReplay) {
		cur, gerr := s.Repo.Get(ctx, id)
		if gerr != nil {
			return nil, gerr
		}
		return &ValidateResult{Connection: cur, Check: res.Check}, nil
	}
	if err != nil {
		return nil, err
	}
	res.Connection = c
	if res.Check.Outcome == Failed {
		return res, &Validation{Issues: res.Check.Issues}
	}
	return res, nil
}

var errReplay = errors.New("replay")

func (s *Service) validateAgainst(ctx context.Context, a Actor, c *Connection, v *registry.ContractVersion) ([]registry.Issue, []capability, error) {
	var is []registry.Issue
	add := func(p, r string) { is = append(is, registry.Issue{Path: p, Rule: r}) }
	now := s.now()
	if v.ConnectorID != c.ConnectorID {
		add("contract_version_id", "the pinned contract belongs to another connector")
	}
	switch v.Status {
	case registry.VersionPublished, registry.VersionSuperseded:
		if v.SupportedUntil != nil && !v.SupportedUntil.After(now) {
			add("contract_version_id", "the pinned contract's support has ended; migrate to a newer version")
		}
	default:
		add("contract_version_id", "the pinned contract version is "+strings.ToLower(v.Status))
	}
	if !containsFold(s.AllowedRegions, c.Region) {
		add("region", "this deployment may not send data to the region")
	}
	for name, info := range v.UsedEndpointProfiles() {
		host := c.Endpoints[name]
		if host == "" || !containsFold(info.Hosts, host) {
			add("endpoints."+name, "no approved host bound")
			continue
		}
		if !containsFold(info.Regions, c.Region) {
			add("region", "the endpoint profile "+name+" does not serve the region")
		}
		// DNS, address ranges and ports are checked against egress policy before any credential could be attached
		if _, err := s.Egress.CheckDestination(ctx, "https://"+host+"/"); err != nil {
			add("endpoints."+name, "the destination fails egress policy")
		}
	}
	used := v.UsedAuthProfiles()
	names := make([]string, 0, len(used))
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	ready := map[string]string{}
	for _, name := range names {
		info := used[name]
		b := c.BindingFor(name)
		if b == nil {
			add("bindings."+name, "no credential bound")
			ready[name] = "no credential bound"
			continue
		}
		if b.Status == BindingRevoked || b.Status == BindingExpired || (b.ExpiresAt != nil && !b.ExpiresAt.After(now)) {
			add("bindings."+name, "the credential has expired or been revoked")
			ready[name] = "credential expired"
			continue
		}
		if missing := difference(lowerSet(info.ScopesRequired), b.GrantedScopes); len(missing) > 0 {
			add("bindings."+name, "granted scopes no longer cover the contract's required scopes")
			ready[name] = "scopes insufficient"
			continue
		}
		if err := s.Vault.Verify(ctx, b.CurrentRef, s.caller(ctx, a)); err != nil {
			if errors.Is(err, vault.ErrUnavailable) {
				return nil, nil, fmt.Errorf("%w: secret vault", ErrDependency)
			}
			add("bindings."+name, "the vault no longer confirms the credential reference")
			ready[name] = "credential unconfirmed"
		}
	}
	var caps []capability
	for _, o := range v.Content.Operations {
		cp := capability{Operation: o.ID, Direction: o.Direction, Ready: true}
		if blocker, bad := ready[o.AuthProfile]; bad {
			cp.Ready, cp.Blocker = false, blocker
		}
		caps = append(caps, cp)
	}
	return is, caps, nil
}

// ── probe ────────────────────────────────────────────────────────────────────

// ProbeInput records the result of an external identity and capability probe. The raw account reference is hashed
// on arrival and never stored.
type ProbeInput struct {
	AccountRef   string   `json:"account_ref"`
	Capabilities []string `json:"capabilities"`
}

// ReportProbe records that the provider account behind the connection was verified: the identity the provider
// reported matches the one the connection was created for, and it offers the contract's operations. Until the
// outbound engine runs probes itself (Wave 4) an authorized operator or system reports them; the evidence is what
// activation of a production connection depends on.
func (s *Service) ReportProbe(ctx context.Context, a Actor, id string, expected int64, in ProbeInput) (*Check, error) {
	snap, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v, err := s.pinned(ctx, snap.ContractVersionID)
	if err != nil {
		return nil, err
	}
	var res *Check
	_, err = s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if expected != 0 && tx.C.Version != expected {
			return ErrVersionConflict
		}
		switch tx.C.Status {
		case StatusAuthorizing, StatusValidating, StatusActive, StatusDegraded:
		default:
			return ErrTransition
		}
		var is []registry.Issue
		if HashAccountRef(tx.C.TenantID, tx.C.ConnectorID, in.AccountRef) != tx.C.ProviderAccountHash {
			is = append(is, registry.Issue{Path: "account_ref", Rule: "the provider account is not the one this connection was created for"})
		}
		have := map[string]bool{}
		for _, c := range in.Capabilities {
			have[c] = true
		}
		for _, o := range v.Content.Operations {
			if !have[o.ID] {
				is = append(is, registry.Issue{Path: "capabilities." + o.ID, Rule: "the provider does not offer this operation"})
			}
		}
		now := s.now()
		ck := &Check{CheckID: uuid.NewString(), Kind: CheckProbe, Outcome: Passed, ConfigFingerprint: ConfigFingerprint(tx.C), Issues: is, RunBy: a.PrincipalID, At: now}
		if len(is) > 0 {
			ck.Outcome = Failed
		}
		det, _ := json.Marshal(map[string]any{"capabilities": lowerSet(in.Capabilities), "account_hash_matches": len(is) == 0 || !hasPath(is, "account_ref")})
		ck.Detail = det
		tx.NewCheck, res = ck, ck
		record(tx, a, "PROBE", tx.C.Status, tx.C.Status, ck.Outcome, now)
		tx.Out = append(tx.Out, out(a, "integration.connection.probed", tx.C, map[string]any{"outcome": ck.Outcome}))
		return nil
	})
	return res, err
}

func hasPath(is []registry.Issue, p string) bool {
	for _, i := range is {
		if i.Path == p {
			return true
		}
	}
	return false
}

// ── certification ────────────────────────────────────────────────────────────

// CertifyInput is a certification run's result.
type CertifyInput struct {
	TestPackVersion string   `json:"test_pack_version"`
	Passed          []string `json:"passed"`
	Outcome         string   `json:"outcome,omitempty"` // PASSED (default) | FAILED
}

// CertificationState says whether a connection's latest certification still covers what it runs (I-29). It is
// computed, not stored: nothing has to remember to invalidate it.
func CertificationState(c *Connection, v *registry.ContractVersion, certs []Certification, now time.Time) (string, *Certification) {
	if c.Environment != EnvProduction {
		return CertNotRequired, nil
	}
	if len(certs) == 0 {
		return CertMissing, nil
	}
	latest := certs[len(certs)-1]
	if latest.Outcome != Passed {
		return CertFailed, &latest
	}
	if v != nil && latest.ScopeFingerprint != CertFingerprint(c, v) {
		return CertStale, &latest
	}
	if !latest.ExpiresAt.After(now) {
		return CertExpired, &latest
	}
	return CertValid, &latest
}

// Certify records a certification run for the connection's pinned contract in its environment. Only someone other
// than the connection's creator certifies it.
func (s *Service) Certify(ctx context.Context, a Actor, id string, in CertifyInput) (*Certification, error) {
	snap, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v, err := s.pinned(ctx, snap.ContractVersionID)
	if err != nil {
		return nil, err
	}
	info := v.Certification()
	if info == nil {
		return nil, ErrContractUnavailable
	}
	if in.TestPackVersion != info.TestPackVersion {
		return nil, bad("test_pack_version", "must be the contract's certification test pack")
	}
	outcome := strings.ToUpper(strings.TrimSpace(in.Outcome))
	if outcome == "" {
		outcome = Passed
	}
	if outcome != Passed && outcome != Failed {
		return nil, bad("outcome", "PASSED or FAILED")
	}
	if outcome == Passed {
		have := map[string]bool{}
		for _, p := range in.Passed {
			have[p] = true
		}
		for _, need := range info.RequiredNegativeTests {
			if !have[need] {
				return nil, bad("passed", need+" has not passed")
			}
		}
	}
	var cert *Certification
	_, err = s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		switch tx.C.Status {
		case StatusDraft, StatusRevoked:
			return ErrTransition
		}
		if a.PrincipalID == tx.C.CreatedBy {
			return ErrSelfAction
		}
		now := s.now()
		cert = &Certification{CertificationID: uuid.NewString(), ConnectionID: tx.C.ConnectionID, Environment: tx.C.Environment,
			ContractVersionID: tx.C.ContractVersionID, ScopeFingerprint: CertFingerprint(tx.C, v), TestPackVersion: in.TestPackVersion,
			PassedTests: lowerKeep(in.Passed), Outcome: outcome, CertifiedBy: a.PrincipalID, At: now,
			ExpiresAt: now.Add(time.Duration(info.ExpiryDays) * 24 * time.Hour)}
		tx.NewCert = cert
		record(tx, a, "CERTIFY", tx.C.Status, tx.C.Status, outcome, now)
		if outcome == Passed {
			tx.Out = append(tx.Out, out(a, "integration.certification.passed", tx.C, map[string]any{
				"certification_run_id": cert.CertificationID, "test_pack_version": cert.TestPackVersion, "expires_at": cert.ExpiresAt.Format(time.RFC3339)}))
		}
		return nil
	})
	return cert, err
}

func lowerKeep(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// ── activate, suspend, resume, revoke ────────────────────────────────────────

// Activate makes a validated connection ACTIVE. The activator is not the creator; the latest validation passed for
// exactly this configuration; a production connection also has a passed probe of its provider account and a valid
// certification.
func (s *Service) Activate(ctx context.Context, a Actor, id string, expected int64) (*Connection, error) {
	snap, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v, err := s.pinned(ctx, snap.ContractVersionID)
	if err != nil {
		return nil, err
	}
	return s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if tx.C.Version != expected {
			return ErrVersionConflict
		}
		if tx.C.Status != StatusValidating {
			return ErrTransition
		}
		if a.PrincipalID == tx.C.CreatedBy {
			return ErrSelfAction
		}
		now := s.now()
		if v.EffectiveFrom.After(now) {
			return fmt.Errorf("%w: the pinned contract is not effective until %s", ErrContractUnavailable, v.EffectiveFrom.Format(time.RFC3339))
		}
		fp := ConfigFingerprint(tx.C)
		lastVal, lastProbe := latestOf(tx.Checks, CheckValidation), latestOf(tx.Checks, CheckProbe)
		if lastVal == nil || lastVal.Outcome != Passed || lastVal.ConfigFingerprint != fp {
			return ErrNotValidated
		}
		if tx.C.Environment == EnvProduction {
			if lastProbe == nil || lastProbe.Outcome != Passed || lastProbe.ConfigFingerprint != fp {
				return fmt.Errorf("%w: the provider account has no passed identity probe for this configuration", ErrNotValidated)
			}
			if st, _ := CertificationState(tx.C, v, tx.Certs, now); st != CertValid {
				return fmt.Errorf("%w: certification is %s", ErrNotCertified, strings.ToLower(st))
			}
		}
		tx.C.Status, tx.C.StatusReason, tx.C.ConfigFingerprint = StatusActive, "", fp
		tx.C.ActivatedBy, tx.C.ActivatedAt, tx.C.SuspendedAt = a.PrincipalID, &now, nil
		record(tx, a, "ACTIVATE", StatusValidating, StatusActive, "", now)
		tx.Out = append(tx.Out, out(a, "integration.connection.activated", tx.C, map[string]any{"region_profile": tx.C.Region, "environment": tx.C.Environment}))
		return nil
	})
}

func latestOf(cs []Check, kind string) *Check {
	for i := len(cs) - 1; i >= 0; i-- {
		if cs[i].Kind == kind {
			return &cs[i]
		}
	}
	return nil
}

// Suspend stops a live connection. New exchanges are refused from the moment it commits.
func (s *Service) Suspend(ctx context.Context, a Actor, id string, expected int64, reason string) (*Connection, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 255 {
		return nil, bad("reason", "required, at most 255 characters")
	}
	return s.suspendWith(ctx, a, id, expected, reason, "SUSPEND")
}

func (s *Service) suspendWith(ctx context.Context, a Actor, id string, expected int64, reason, kind string) (*Connection, error) {
	return s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if expected != 0 && tx.C.Version != expected {
			return ErrVersionConflict
		}
		if !tx.C.Live() {
			return ErrTransition
		}
		now := s.now()
		from := tx.C.Status
		tx.C.Status, tx.C.StatusReason, tx.C.SuspendedAt = StatusSuspended, reason, &now
		record(tx, a, kind, from, StatusSuspended, reason, now)
		tx.Out = append(tx.Out, out(a, "integration.connection.suspended", tx.C, map[string]any{"reason_code": reasonCode(reason), "affected_operations": "all"}))
		return nil
	})
}

func reasonCode(r string) string {
	r = strings.ToUpper(strings.TrimSpace(r))
	if i := strings.IndexAny(r, " :"); i > 0 {
		r = r[:i]
	}
	return r
}

// Resume moves a suspended connection back to VALIDATING: it must be validated and activated again, because
// whatever caused the suspension may have changed what is true.
func (s *Service) Resume(ctx context.Context, a Actor, id string, expected int64) (*Connection, error) {
	return s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if tx.C.Version != expected {
			return ErrVersionConflict
		}
		if tx.C.Status != StatusSuspended {
			return ErrTransition
		}
		now := s.now()
		tx.C.Status, tx.C.StatusReason = StatusValidating, ""
		record(tx, a, "RESUME", StatusSuspended, StatusValidating, "", now)
		tx.Out = append(tx.Out, out(a, "integration.connection.resumed", tx.C, nil))
		return nil
	})
}

// Revoke ends a connection for good. Operational access stops at once; the metadata and evidence stay.
func (s *Service) Revoke(ctx context.Context, a Actor, id string, expected int64, reason string) (*Connection, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 255 {
		return nil, bad("reason", "required, at most 255 characters")
	}
	return s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if tx.C.Version != expected {
			return ErrVersionConflict
		}
		if tx.C.Status == StatusRevoked {
			return ErrTransition
		}
		now := s.now()
		from := tx.C.Status
		tx.C.Status, tx.C.StatusReason, tx.C.RevokedAt = StatusRevoked, reason, &now
		for i := range tx.C.Bindings {
			tx.C.Bindings[i].Status = BindingRevoked
		}
		record(tx, a, "REVOKE", from, StatusRevoked, reason, now)
		tx.Out = append(tx.Out, out(a, "integration.connection.revoked", tx.C, map[string]any{"reason_code": reasonCode(reason)}))
		return nil
	})
}

// ── scope reports ────────────────────────────────────────────────────────────

// ReportScopes records what the provider says an account's scopes now are. A widening is a material authorization
// change and suspends the connection for review; a narrowing below what the contract needs blocks the operations
// that depend on it.
func (s *Service) ReportScopes(ctx context.Context, a Actor, id string, expected int64, profile string, scopes []string) (*Connection, error) {
	reported := lowerSet(scopes)
	snap, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v, err := s.pinned(ctx, snap.ContractVersionID)
	if err != nil {
		return nil, err
	}
	info, ok := v.UsedAuthProfiles()[profile]
	if !ok {
		return nil, bad("auth_profile", "the pinned contract has no such authentication profile")
	}
	return s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if expected != 0 && tx.C.Version != expected {
			return ErrVersionConflict
		}
		b := tx.C.BindingFor(profile)
		if b == nil {
			return bad("auth_profile", "no credential bound for this profile")
		}
		if !tx.C.Live() && tx.C.Status != StatusAuthorizing && tx.C.Status != StatusValidating {
			return ErrTransition
		}
		now := s.now()
		added, removed := difference(reported, b.GrantedScopes), difference(b.GrantedScopes, reported)
		if len(added) == 0 && len(removed) == 0 {
			return nil
		}
		b.GrantedScopes, b.ScopeFingerprint = reported, ScopeFingerprint(reported)
		tx.C.ConfigFingerprint = ConfigFingerprint(tx.C)
		from := tx.C.Status
		tx.Out = append(tx.Out, out(a, "integration.connection.scopes-changed", tx.C,
			map[string]any{"auth_profile": profile, "scopes_added": len(added), "scopes_removed": len(removed)}))
		switch {
		case len(difference(lowerSet(info.ScopesRequired), reported)) > 0 && tx.C.Status == StatusActive:
			tx.C.Status, tx.C.StatusReason = StatusDegraded, "SCOPE_REDUCED"
		case len(difference(reported, lowerSet(info.ScopesRequired))) > 0 && tx.C.Live():
			// wider than the contract needs: a material authorization change that someone must review
			tx.C.Status, tx.C.StatusReason, tx.C.SuspendedAt = StatusSuspended, "SCOPE_EXPANDED", &now
			tx.Out = append(tx.Out, out(a, "integration.connection.suspended", tx.C, map[string]any{"reason_code": "SCOPE_EXPANDED", "affected_operations": "all"}))
		}
		record(tx, a, "SCOPES_CHANGED", from, tx.C.Status, profile, now)
		return nil
	})
}

// ── rotation ─────────────────────────────────────────────────────────────────

// RotateInput replaces a binding's credential.
type RotateInput struct {
	AuthProfile  string     `json:"auth_profile"`
	NewRef       string     `json:"new_credential_ref"`
	GraceSeconds int64      `json:"grace_seconds"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// Rotate cuts a binding over to a new credential reference. The new reference is confirmed by the vault BEFORE the
// cutover; the old one stays only for an explicit, bounded grace window and is never tried as a fallback.
func (s *Service) Rotate(ctx context.Context, a Actor, id string, expected int64, in RotateInput) (*Connection, error) {
	tenant := tenantOf(ctx)
	if !domain.ValidCredentialRef(in.NewRef) {
		return nil, bad("new_credential_ref", "must be a vault secret policy id")
	}
	grace := time.Duration(in.GraceSeconds) * time.Second
	if in.GraceSeconds < 0 || grace > s.cfg().MaxGrace {
		return nil, bad("grace_seconds", fmt.Sprintf("between 0 and %d", int64(s.cfg().MaxGrace/time.Second)))
	}
	snap, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if snap.Version != expected {
		return nil, ErrVersionConflict
	}
	b := snap.BindingFor(in.AuthProfile)
	if b == nil {
		return nil, bad("auth_profile", "no credential bound for this profile")
	}
	now := s.now()
	if strings.EqualFold(in.NewRef, b.CurrentRef) || strings.EqualFold(in.NewRef, b.PreviousRef) {
		return nil, bad("new_credential_ref", "must differ from the current and previous references")
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		return nil, ErrCredentialExpired
	}
	if err := s.mapVault(s.Vault.Verify(ctx, in.NewRef, s.caller(ctx, a))); err != nil {
		return nil, err
	}
	c, err := s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if tx.C.Version != expected {
			return ErrVersionConflict
		}
		if !tx.C.Live() {
			return ErrTransition
		}
		cur := tx.C.BindingFor(in.AuthProfile)
		if cur == nil {
			return ErrTransition
		}
		if cur.PreviousRef != "" && cur.PreviousGraceUntil != nil && cur.PreviousGraceUntil.After(now) {
			return fmt.Errorf("%w: the previous rotation's grace window is still open", ErrRotation)
		}
		old := cur.CurrentRef
		cur.CurrentRef, cur.PreviousRef, cur.PreviousGraceUntil = strings.ToLower(in.NewRef), "", nil
		if grace > 0 {
			until := now.Add(grace)
			cur.PreviousRef, cur.PreviousGraceUntil = old, &until
		}
		cur.ExpiresAt, cur.Status = in.ExpiresAt, BindingOK
		due := now.Add(s.cfg().RotationInterval)
		if in.ExpiresAt != nil && in.ExpiresAt.Before(due) {
			due = *in.ExpiresAt
		}
		cur.RotationDueAt, cur.BoundBy, cur.BoundAt = &due, a.PrincipalID, now
		fp := ConfigFingerprint(tx.C)
		tx.C.ConfigFingerprint = fp
		// the new reference was confirmed before cutover, so the rotation is itself a passed validation of this
		// configuration; the activation evidence stays consistent
		tx.NewCheck = &Check{CheckID: uuid.NewString(), Kind: CheckValidation, Outcome: Passed, ConfigFingerprint: fp, RunBy: a.PrincipalID, At: now,
			Detail: json.RawMessage(`{"reason":"credential rotation, new reference confirmed before cutover"}`)}
		record(tx, a, "ROTATE", tx.C.Status, tx.C.Status, in.AuthProfile, now)
		tx.Out = append(tx.Out, out(a, "integration.connection.credential-rotated", tx.C,
			map[string]any{"auth_profile": in.AuthProfile, "grace_seconds": in.GraceSeconds}))
		return nil
	})
	if errors.Is(err, ErrCrossTenantCredential) {
		_ = s.Repo.RecordSignal(ctx, tenant, OutEvent{Type: "integration.security.cross-tenant-credential", AggregateType: "connection",
			AggregateID: id, ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
			Data: map[string]any{"tenant_id": tenant, "connection_id": id, "auth_profile": in.AuthProfile, "reason_code": "XIC-008"}})
	}
	return c, err
}

// EndGrace closes an open rotation grace window early.
func (s *Service) EndGrace(ctx context.Context, a Actor, id string, expected int64, profile string) (*Connection, error) {
	return s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if tx.C.Version != expected {
			return ErrVersionConflict
		}
		b := tx.C.BindingFor(profile)
		if b == nil || b.PreviousRef == "" {
			return ErrTransition
		}
		b.PreviousRef, b.PreviousGraceUntil = "", nil
		record(tx, a, "END_GRACE", tx.C.Status, tx.C.Status, profile, s.now())
		return nil
	})
}

// ── migration (I-04) ─────────────────────────────────────────────────────────

// MigrationInput requests moving a connection to another contract version.
type MigrationInput struct {
	ToContractVersionID string     `json:"to_contract_version_id"`
	EffectiveAt         *time.Time `json:"effective_at,omitempty"`
	Reason              string     `json:"reason"`
}

type migrationPayload struct {
	From string `json:"from_contract_version_id"`
	To   string `json:"to_contract_version_id"`
}

// RequestMigration asks to move a connection from the contract version it accepted to a newer PUBLISHED one. The
// connection stays pinned until the request is approved by someone else and its effective date arrives.
func (s *Service) RequestMigration(ctx context.Context, a Actor, id string, in MigrationInput) (*Request, error) {
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 255 {
		return nil, bad("reason", "required, at most 255 characters")
	}
	snap, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if snap.Status == StatusRevoked {
		return nil, ErrTransition
	}
	cur, err := s.pinned(ctx, snap.ContractVersionID)
	if err != nil {
		return nil, err
	}
	to, err := s.pinned(ctx, in.ToContractVersionID)
	if err != nil {
		return nil, err
	}
	switch {
	case to.ConnectorID != snap.ConnectorID:
		return nil, bad("to_contract_version_id", "belongs to another connector")
	case to.Status != registry.VersionPublished:
		return nil, bad("to_contract_version_id", "migration targets a PUBLISHED contract version")
	case to.ContractVersionID == cur.ContractVersionID:
		return nil, bad("to_contract_version_id", "the connection is already pinned to this version")
	}
	// the new contract must be servable by what the connection already holds: every authentication profile it
	// uses is bound and every endpoint profile has an approved host bound. Anything else is a new authorization.
	for name := range to.UsedAuthProfiles() {
		if snap.BindingFor(name) == nil {
			return nil, bad("to_contract_version_id", "the target contract needs a credential for "+name+"; authorize it first")
		}
	}
	for name, info := range to.UsedEndpointProfiles() {
		if h := snap.Endpoints[name]; h == "" || !containsFold(info.Hosts, h) || !containsFold(info.Regions, snap.Region) {
			return nil, bad("to_contract_version_id", "the target contract's endpoint "+name+" is not served by this connection's binding")
		}
	}
	now := s.now()
	if in.EffectiveAt != nil && in.EffectiveAt.Before(now.Add(-time.Minute)) {
		return nil, bad("effective_at", "cannot be in the past")
	}
	payload, _ := json.Marshal(migrationPayload{From: cur.ContractVersionID, To: to.ContractVersionID})
	var req *Request
	_, err = s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		for _, r := range tx.Requests {
			if r.Kind == RequestMigration && (r.Status == ReqPending || r.Status == ReqApproved) {
				return fmt.Errorf("%w: another migration is already in progress", ErrRequestState)
			}
		}
		req = &Request{RequestID: uuid.NewString(), ConnectionID: id, Kind: RequestMigration, Payload: payload, Reason: in.Reason,
			Status: ReqPending, RequestedBy: a.PrincipalID, EffectiveAt: in.EffectiveAt, CreatedAt: now}
		tx.NewRequest = req
		record(tx, a, "MIGRATION_REQUESTED", tx.C.Status, tx.C.Status, in.Reason, now)
		tx.Out = append(tx.Out, out(a, "integration.connection.migration-requested", tx.C, map[string]any{"to_contract_version_id": to.ContractVersionID}))
		return nil
	})
	return req, err
}

// DecideMigration approves or rejects a migration request. An approval applies at once when its effective date has
// arrived; otherwise it is applied when the date does (see Evaluate) and Operable resolves the pin by date.
func (s *Service) DecideMigration(ctx context.Context, a Actor, id, requestID string, approve bool) (*Request, error) {
	var dec *Request
	_, err := s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		var req *Request
		for i := range tx.Requests {
			if tx.Requests[i].RequestID == requestID && tx.Requests[i].Kind == RequestMigration {
				req = &tx.Requests[i]
			}
		}
		if req == nil {
			return ErrRequestNotFound
		}
		if req.Status != ReqPending {
			return ErrRequestState
		}
		if a.PrincipalID == req.RequestedBy {
			return ErrSelfAction
		}
		now := s.now()
		req.DecidedBy, req.DecidedAt = a.PrincipalID, &now
		var p migrationPayload
		_ = json.Unmarshal(req.Payload, &p)
		if !approve {
			req.Status = ReqRejected
			record(tx, a, "MIGRATION_REJECTED", tx.C.Status, tx.C.Status, "", now)
		} else {
			req.Status = ReqApproved
			if req.EffectiveAt == nil || !req.EffectiveAt.After(now) {
				req.Status = ReqUsed
				tx.ApplyMigration = p.To
				tx.C.ContractVersionID = p.To
				tx.C.ConfigFingerprint = ConfigFingerprint(tx.C)
				tx.Out = append(tx.Out, out(a, "integration.connection.migrated", tx.C, map[string]any{"from_contract_version_id": p.From}))
			}
			record(tx, a, "MIGRATION_APPROVED", tx.C.Status, tx.C.Status, "", now)
		}
		tx.UpdateRequest, dec = req, req
		return nil
	})
	return dec, err
}

// EffectivePin is the contract version a connection runs against at a moment: the version it accepted, or the
// target of an approved migration whose effective date has arrived.
func EffectivePin(c *Connection, reqs []Request, now time.Time) string {
	pin := c.ContractVersionID
	for _, r := range reqs {
		if r.Kind == RequestMigration && r.Status == ReqApproved && r.EffectiveAt != nil && !r.EffectiveAt.After(now) {
			var p migrationPayload
			if json.Unmarshal(r.Payload, &p) == nil && p.To != "" {
				pin = p.To
			}
		}
	}
	return pin
}

// ── break-glass ──────────────────────────────────────────────────────────────

// BreakGlassInput requests a support repair.
type BreakGlassInput struct {
	Action          string `json:"action"` // PAUSE | RESUME
	Reason          string `json:"reason"`
	DurationSeconds int64  `json:"duration_seconds"`
}

// RequestBreakGlass asks for a support action. A pause is immediate (stopping traffic is the safe direction); a
// resume needs approval from someone else and is time-bounded. Revealing a credential, or anything else, is
// refused and the attempt is itself evidenced: break-glass never crosses the secret-custody boundary (NP-50).
func (s *Service) RequestBreakGlass(ctx context.Context, a Actor, id string, expected int64, in BreakGlassInput) (*Request, *Connection, error) {
	tenant := tenantOf(ctx)
	action := strings.ToUpper(strings.TrimSpace(in.Action))
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 255 {
		return nil, nil, bad("reason", "required, at most 255 characters")
	}
	if action != "PAUSE" && action != "RESUME" {
		_ = s.Repo.RecordSignal(ctx, tenant, OutEvent{Type: "integration.connection.break-glass-denied", AggregateType: "connection", AggregateID: id,
			ActorID: a.PrincipalID, CorrelationID: a.CorrelationID,
			Data: map[string]any{"tenant_id": tenant, "connection_id": id, "reason_code": "SECRET_CUSTODY"}})
		return nil, nil, ErrBreakGlassDenied
	}
	dur := time.Duration(in.DurationSeconds) * time.Second
	if action == "RESUME" && (dur <= 0 || dur > s.cfg().BreakGlassMax) {
		return nil, nil, bad("duration_seconds", fmt.Sprintf("between 1 and %d", int64(s.cfg().BreakGlassMax/time.Second)))
	}
	if action == "PAUSE" {
		c, err := s.suspendWith(ctx, a, id, expected, "BREAK_GLASS: "+in.Reason, "BREAK_GLASS_PAUSE")
		return nil, c, err
	}
	var req *Request
	c, err := s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		if tx.C.Status != StatusSuspended {
			return ErrTransition
		}
		now := s.now()
		exp := now.Add(dur)
		payload, _ := json.Marshal(map[string]string{"action": action})
		req = &Request{RequestID: uuid.NewString(), ConnectionID: id, Kind: RequestBreakGlass, Payload: payload, Reason: in.Reason,
			Status: ReqPending, RequestedBy: a.PrincipalID, ExpiresAt: &exp, CreatedAt: now}
		tx.NewRequest = req
		record(tx, a, "BREAK_GLASS_REQUESTED", tx.C.Status, tx.C.Status, in.Reason, now)
		tx.Out = append(tx.Out, out(a, "integration.connection.break-glass-requested", tx.C, map[string]any{"action": action}))
		return nil
	})
	return req, c, err
}

// DecideBreakGlass approves or rejects a break-glass resume. Approval moves the connection to VALIDATING; it
// still has to be validated and activated, so break-glass cannot skip a gate.
func (s *Service) DecideBreakGlass(ctx context.Context, a Actor, id, requestID string, approve bool) (*Request, error) {
	var dec *Request
	_, err := s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		var req *Request
		for i := range tx.Requests {
			if tx.Requests[i].RequestID == requestID && tx.Requests[i].Kind == RequestBreakGlass {
				req = &tx.Requests[i]
			}
		}
		if req == nil {
			return ErrRequestNotFound
		}
		if req.Status != ReqPending {
			return ErrRequestState
		}
		if a.PrincipalID == req.RequestedBy {
			return ErrSelfAction
		}
		now := s.now()
		req.DecidedBy, req.DecidedAt = a.PrincipalID, &now
		switch {
		case req.ExpiresAt != nil && !req.ExpiresAt.After(now):
			req.Status = ReqExpired
		case !approve:
			req.Status = ReqRejected
		default:
			req.Status = ReqUsed
			if tx.C.Status != StatusSuspended {
				return ErrTransition
			}
			tx.C.Status, tx.C.StatusReason = StatusValidating, ""
			record(tx, a, "BREAK_GLASS_RESUME", StatusSuspended, StatusValidating, req.Reason, now)
			tx.Out = append(tx.Out, out(a, "integration.connection.resumed", tx.C, map[string]any{"via": "break-glass"}))
		}
		tx.UpdateRequest, dec = req, req
		return nil
	})
	if err == nil && dec != nil && dec.Status == ReqExpired {
		return dec, ErrRequestState
	}
	return dec, err
}

// ── evaluation (expiry-driven behavior) ──────────────────────────────────────

// EvaluateResult reports what an evaluation changed.
type EvaluateResult struct {
	Connection *Connection `json:"connection"`
	Changes    []string    `json:"changes"`
}

// Evaluate applies the time-driven rules to one connection: credential expiry and rotation overdue, an ended
// rotation grace window, a pinned contract whose support ended, and an approved migration whose date arrived.
// Expiry drives DEGRADED or SUSPENDED; it never silently extends anything.
func (s *Service) Evaluate(ctx context.Context, a Actor, id string) (*EvaluateResult, error) {
	var changes []string
	c, err := s.Repo.Mutate(ctx, id, func(tx *Tx) error {
		now := s.now()
		cfg := s.cfg()
		note := func(m string) { changes = append(changes, m) }
		if tx.C.Status == StatusRevoked {
			return errNoChange
		}
		// an approved migration whose date arrived
		for i := range tx.Requests {
			r := &tx.Requests[i]
			if r.Kind == RequestMigration && r.Status == ReqApproved && r.EffectiveAt != nil && !r.EffectiveAt.After(now) && tx.UpdateRequest == nil {
				var p migrationPayload
				_ = json.Unmarshal(r.Payload, &p)
				r.Status = ReqUsed
				tx.UpdateRequest, tx.ApplyMigration = r, p.To
				tx.C.ContractVersionID = p.To
				tx.C.ConfigFingerprint = ConfigFingerprint(tx.C)
				tx.Out = append(tx.Out, out(a, "integration.connection.migrated", tx.C, map[string]any{"from_contract_version_id": p.From}))
				note("MIGRATION_APPLIED")
			}
		}
		expiredAll, anyLive := true, false
		for i := range tx.C.Bindings {
			b := &tx.C.Bindings[i]
			if b.Status == BindingRevoked {
				continue
			}
			anyLive = true
			if b.PreviousRef != "" && b.PreviousGraceUntil != nil && !b.PreviousGraceUntil.After(now) {
				b.PreviousRef, b.PreviousGraceUntil = "", nil
				note("GRACE_ENDED:" + b.AuthProfile)
			}
			prev := b.Status
			switch {
			case b.ExpiresAt != nil && !b.ExpiresAt.After(now):
				b.Status = BindingExpired
			case b.ExpiresAt != nil && b.ExpiresAt.Sub(now) <= cfg.ExpiryWarning:
				b.Status = BindingExpiring
			default:
				b.Status = BindingOK
			}
			if b.Status != BindingExpired {
				expiredAll = false
			}
			if b.Status != prev {
				note("BINDING_" + b.Status + ":" + b.AuthProfile)
				tx.Out = append(tx.Out, out(a, "integration.connection.credential-"+strings.ToLower(b.Status), tx.C, map[string]any{"auth_profile": b.AuthProfile}))
			}
			if b.RotationDueAt != nil && !b.RotationDueAt.After(now) && b.Status == BindingOK {
				note("ROTATION_OVERDUE:" + b.AuthProfile)
			}
		}
		if tx.C.Live() && anyLive {
			from := tx.C.Status
			switch {
			case expiredAll:
				tx.C.Status, tx.C.StatusReason, tx.C.SuspendedAt = StatusSuspended, "CREDENTIALS_EXPIRED", &now
				tx.Out = append(tx.Out, out(a, "integration.connection.suspended", tx.C, map[string]any{"reason_code": "CREDENTIALS_EXPIRED", "affected_operations": "all"}))
			case tx.C.Status == StatusActive && hasStatus(tx.C.Bindings, BindingExpired):
				tx.C.Status, tx.C.StatusReason = StatusDegraded, "CREDENTIAL_EXPIRED"
			case tx.C.Status == StatusActive && hasStatus(tx.C.Bindings, BindingExpiring):
				tx.C.Status, tx.C.StatusReason = StatusDegraded, "CREDENTIAL_EXPIRING"
			case tx.C.Status == StatusDegraded && credentialReason(tx.C.StatusReason) && hasStatus(tx.C.Bindings, BindingExpired):
				tx.C.StatusReason = "CREDENTIAL_EXPIRED"
			case tx.C.Status == StatusDegraded && credentialReason(tx.C.StatusReason) && hasStatus(tx.C.Bindings, BindingExpiring):
				tx.C.StatusReason = "CREDENTIAL_EXPIRING"
			case tx.C.Status == StatusDegraded && credentialReason(tx.C.StatusReason):
				tx.C.Status, tx.C.StatusReason = StatusActive, ""
			}
			if tx.C.Status != from {
				note("STATUS:" + from + ">" + tx.C.Status)
			}
		}
		if len(changes) == 0 {
			return errNoChange
		}
		record(tx, a, "EVALUATE", "", tx.C.Status, strings.Join(changes, ","), now)
		return nil
	})
	if errors.Is(err, errNoChange) {
		cur, gerr := s.Repo.Get(ctx, id)
		return &EvaluateResult{Connection: cur, Changes: []string{}}, gerr
	}
	if err != nil {
		return nil, err
	}
	return &EvaluateResult{Connection: c, Changes: changes}, nil
}

var errNoChange = errors.New("no change")

func hasStatus(bs []Binding, st string) bool {
	for _, b := range bs {
		if b.Status == st {
			return true
		}
	}
	return false
}

// ── operability: the gate every exchange passes (I-01) ───────────────────────

// Decision is what a successful Operable returns to server-side callers (inbound gateway, outbound engine). It
// carries the credential reference an exchange will need; the HTTP view never does.
type Decision struct {
	ConnectionID      string             `json:"connection_id"`
	TenantID          string             `json:"tenant_id"`
	Environment       string             `json:"environment"`
	ConnectorID       string             `json:"connector_id"`
	ContractVersionID string             `json:"contract_version_id"`
	Operation         registry.Operation `json:"operation"`
	Region            string             `json:"region"`
	Host              string             `json:"host,omitempty"`
	CredentialRefs    []CredentialUse    `json:"-"`
	Degraded          bool               `json:"degraded,omitempty"`
}

// CredentialUse is one reference an exchange may use, labelled so the caller never tries every secret.
type CredentialUse struct {
	Ref   string
	Grace bool
}

// Operable decides whether the tenant may run an operation of this connection right now. Every refusal is a
// stable XIC error: XIC-008 for a connection that belongs to someone else, XIC-001 for one that is not active,
// XIC-002 for a contract the connection is not pinned to, XIC-016 for a region the operation does not allow,
// XIC-017 for a production certification that no longer covers what runs.
func (s *Service) Operable(ctx context.Context, tenant, connectionID, operationID string) (*Decision, error) {
	c, err := s.Repo.GetAcrossTenants(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	if c.TenantID != tenant {
		_ = s.Repo.RecordSignal(middleware.WithTenant(ctx, tenant), tenant, OutEvent{Type: "integration.security.tenant-scope-mismatch",
			AggregateType: "connection", AggregateID: connectionID, ActorID: "system",
			Data: map[string]any{"tenant_id": tenant, "connection_id": connectionID, "reason_code": "XIC-008"}})
		return nil, &Refusal{Code: CodeTenantScope, Detail: "the connection belongs to another tenant"}
	}
	if !c.Live() {
		return nil, &Refusal{Code: CodeNotActive, Detail: "the connection is " + strings.ToLower(c.Status), ConnectionID: c.ConnectionID}
	}
	now := s.now()
	reqs, err := s.Repo.ListRequests(withTenantCtx(ctx, c.TenantID), c.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	pin := EffectivePin(c, reqs, now)
	v, err := s.pinned(withTenantCtx(ctx, c.TenantID), pin)
	if err != nil {
		return nil, &Refusal{Code: CodeContractMismatch, Detail: "the pinned contract version is not available", ConnectionID: c.ConnectionID}
	}
	if v.Status == registry.VersionRetired || v.Status == registry.VersionBlocked || (v.SupportedUntil != nil && !v.SupportedUntil.After(now)) {
		return nil, &Refusal{Code: CodeContractMismatch, Detail: "the pinned contract version is no longer supported", ConnectionID: c.ConnectionID}
	}
	op := v.Operation(operationID)
	if op == nil {
		return nil, &Refusal{Code: CodeContractMismatch, Detail: "the operation is not registered in the pinned contract", ConnectionID: c.ConnectionID}
	}
	if op.Deprecated && op.SupportedUntil != nil && !op.SupportedUntil.After(now) {
		return nil, &Refusal{Code: CodeContractMismatch, Detail: "the operation is no longer supported", ConnectionID: c.ConnectionID}
	}
	if !containsFold(op.AllowedRegions, c.Region) || !containsFold(s.AllowedRegions, c.Region) {
		return nil, &Refusal{Code: CodeResidency, Detail: "the operation may not carry data to this connection's region", ConnectionID: c.ConnectionID}
	}
	if c.Environment == EnvProduction {
		certs, err := s.Repo.ListCertifications(withTenantCtx(ctx, c.TenantID), c.ConnectionID)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependency, err)
		}
		if st, _ := CertificationState(c, v, certs, now); st != CertValid {
			return nil, &Refusal{Code: CodeCertStale, Detail: "the production certification is " + strings.ToLower(st), ConnectionID: c.ConnectionID}
		}
		if s.Stage != nil {
			// last of the gates: a refused operation takes nothing from the pilot budget
			if err := s.Stage.Admit(withTenantCtx(ctx, c.TenantID), c, operationID, now); err != nil {
				return nil, err
			}
		}
	}
	d := &Decision{ConnectionID: c.ConnectionID, TenantID: c.TenantID, Environment: c.Environment, ConnectorID: c.ConnectorID,
		ContractVersionID: v.ContractVersionID, Operation: *op, Region: c.Region, Degraded: c.Status == StatusDegraded}
	if op.EndpointProfile != "" {
		d.Host = c.Endpoints[op.EndpointProfile]
	}
	// the credential an exchange needs: the current reference, plus the previous one only inside its grace window
	info, ok := v.UsedAuthProfiles()[op.AuthProfile]
	b := c.BindingFor(op.AuthProfile)
	if !ok || b == nil {
		return nil, &Refusal{Code: CodeNotActive, Detail: "no credential is bound for this operation", ConnectionID: c.ConnectionID}
	}
	if b.Status == BindingExpired || b.Status == BindingRevoked || (b.ExpiresAt != nil && !b.ExpiresAt.After(now)) {
		return nil, &Refusal{Code: CodeNotActive, Detail: "the credential for this operation has expired", ConnectionID: c.ConnectionID}
	}
	if len(difference(lowerSet(info.ScopesRequired), b.GrantedScopes)) > 0 {
		return nil, &Refusal{Code: CodeNotActive, Detail: "the granted scopes no longer cover this operation", ConnectionID: c.ConnectionID}
	}
	d.CredentialRefs = []CredentialUse{{Ref: b.CurrentRef}}
	if b.PreviousRef != "" && b.PreviousGraceUntil != nil && b.PreviousGraceUntil.After(now) {
		d.CredentialRefs = append(d.CredentialRefs, CredentialUse{Ref: b.PreviousRef, Grace: true})
	}
	return d, nil
}

func withTenantCtx(ctx context.Context, tenant string) context.Context {
	return middleware.WithTenant(ctx, tenant)
}

// Refusal codes: the XIC codes an Operable refusal carries.
type RefusalCode string

const (
	CodeNotActive        RefusalCode = "XIC-001"
	CodeContractMismatch RefusalCode = "XIC-002"
	CodeTenantScope      RefusalCode = "XIC-008"
	CodeRateLimited      RefusalCode = "XIC-012"
	CodeResidency        RefusalCode = "XIC-016"
	CodeCertStale        RefusalCode = "XIC-017"
)

// Refusal is an Operable refusal with its stable code.
type Refusal struct {
	Code         RefusalCode
	Detail       string
	ConnectionID string
}

func (r *Refusal) Error() string { return string(r.Code) + ": " + r.Detail }

func credentialReason(r string) bool { return r == "CREDENTIAL_EXPIRING" || r == "CREDENTIAL_EXPIRED" }

// EvaluateDue applies the time-driven rules to every connection the store says is due, across tenants, one at a
// time under that connection's own tenant scope. It returns how many it looked at; a connection that fails does
// not stop the others.
func (s *Service) EvaluateDue(ctx context.Context, batch int) (int, error) {
	due, err := s.Repo.Due(ctx, s.now(), s.cfg().ExpiryWarning, batch)
	if err != nil {
		return 0, err
	}
	var first error
	for _, d := range due {
		if _, err := s.Evaluate(withTenantCtx(ctx, d.TenantID), Actor{PrincipalID: "system:xic-evaluator"}, d.ConnectionID); err != nil && first == nil {
			first = err
		}
	}
	return len(due), first
}

// RunEvaluator evaluates due connections every interval until ctx ends. report is called with each pass's outcome.
func (s *Service) RunEvaluator(ctx context.Context, interval time.Duration, batch int, report func(n int, err error)) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.EvaluateDue(ctx, batch)
			if report != nil {
				report(n, err)
			}
		}
	}
}

// Connection resolves a connection by id regardless of tenant: the inbound path finds the connection from the
// trusted route first and learns the tenant from it. It reads only.
func (s *Service) Connection(ctx context.Context, id string) (*Connection, error) {
	return s.Repo.GetAcrossTenants(ctx, id)
}

// PinnedContract returns the contract version a connection runs against right now: the version it accepted, or the
// target of an approved migration whose effective date has arrived.
func (s *Service) PinnedContract(ctx context.Context, c *Connection) (*registry.ContractVersion, error) {
	tctx := withTenantCtx(ctx, c.TenantID)
	reqs, err := s.Repo.ListRequests(tctx, c.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	return s.pinned(tctx, EffectivePin(c, reqs, s.now()))
}

// CertStatus is a connection's certification standing: the computed state, the run it rests on and how long it has.
// RetestDue is true when the certification is not VALID for a production connection, or is VALID but inside the
// warning window before it expires, so a retest can be scheduled before production is blocked (OD-11, NP-47).
type CertStatus struct {
	ConnectionID  string         `json:"connection_id"`
	Environment   string         `json:"environment"`
	State         string         `json:"state"`
	Certification *Certification `json:"certification,omitempty"`
	DaysRemaining *int           `json:"days_remaining,omitempty"`
	RetestDue     bool           `json:"retest_due"`
	// Reasons say why a production connection is not covered: the materiality triggers of §7.3 and NP-47/48.
	Reasons []string `json:"reasons,omitempty"`
}

// CertificationStatus reports a connection's certification state against the contract it runs now.
func (s *Service) CertificationStatus(ctx context.Context, id string) (*CertStatus, error) {
	c, err := s.Repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.CertificationStatusOf(ctx, c)
}

// CertificationStatusOf is CertificationStatus for a connection already read.
func (s *Service) CertificationStatusOf(ctx context.Context, c *Connection) (*CertStatus, error) {
	tctx := withTenantCtx(ctx, c.TenantID)
	certs, err := s.Repo.ListCertifications(tctx, c.ConnectionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependency, err)
	}
	v, err := s.PinnedContract(ctx, c)
	if err != nil {
		v = nil // a contract that cannot be read is reported as it is: the state falls back to what the run says
	}
	now := s.now()
	st, latest := CertificationState(c, v, certs, now)
	out := &CertStatus{ConnectionID: c.ConnectionID, Environment: c.Environment, State: st, Certification: latest}
	if latest != nil && st == CertValid {
		d := int(latest.ExpiresAt.Sub(now).Hours() / 24)
		out.DaysRemaining = &d
	}
	if c.Environment == EnvProduction {
		switch st {
		case CertValid:
			out.RetestDue = latest.ExpiresAt.Sub(now) <= s.cfg().ExpiryWarning
			if out.RetestDue {
				out.Reasons = append(out.Reasons, "EXPIRING")
			}
		case CertStale:
			out.RetestDue = true
			out.Reasons = append(out.Reasons, "MATERIAL_CHANGE") // contract/mapping/auth/endpoint change since the run (I-29)
		case CertExpired:
			out.RetestDue = true
			out.Reasons = append(out.Reasons, "EXPIRED")
		case CertMissing:
			out.RetestDue = true
			out.Reasons = append(out.Reasons, "NEVER_CERTIFIED")
		case CertFailed:
			out.RetestDue = true
			out.Reasons = append(out.Reasons, "LAST_RUN_FAILED")
		}
	}
	return out, nil
}
