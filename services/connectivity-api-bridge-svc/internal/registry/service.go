package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"zoiko.io/connectivity-api-bridge-svc/internal/registry/mapping"
)

// Service holds the XIC-01 commands. Every one of them writes only from the platform scope, and every material
// one writes its evidence and its outbox event in the same transaction as the change.
type Service struct {
	Repo   Repo
	Limits Limits
	Clock  func() time.Time
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) limits() Limits {
	if s.Limits == (Limits{}) {
		return DefaultLimits()
	}
	return s.Limits
}

func platform(tenant string) error {
	if tenant != PlatformScopeID {
		return ErrNotPlatformScope
	}
	return nil
}

func ev(a Actor, typ, aggType, aggID string, data map[string]any) Event {
	return Event{Type: typ, AggregateType: aggType, AggregateID: aggID, ActorID: a.PrincipalID, CorrelationID: a.CorrelationID, Data: data}
}

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ── connectors ───────────────────────────────────────────────────────────────

// CreateConnector registers a DRAFT connector identity. The id is never reused.
func (s *Service) CreateConnector(ctx context.Context, tenant string, a Actor, c Connector, idemKey string) (*Connector, bool, error) {
	if err := platform(tenant); err != nil {
		return nil, false, err
	}
	c.ConnectorID = strings.ToLower(strings.TrimSpace(c.ConnectorID))
	c.Family = strings.ToUpper(strings.TrimSpace(c.Family))
	c.ProviderClass, c.Owner = strings.TrimSpace(c.ProviderClass), strings.TrimSpace(c.Owner)
	if err := c.Validate(); err != nil {
		return nil, false, err
	}
	now := s.now()
	c.Lifecycle, c.Version, c.CreatedBy, c.CreatedAt, c.UpdatedAt = ConnectorDraft, 1, a.PrincipalID, now, now
	c.SupportedUntil = nil
	created, err := s.Repo.CreateConnector(ctx, &c, idemKey, hashOf(c.ConnectorID, c.Family, c.ProviderClass, c.Owner),
		ev(a, "integration.connector.created", "connector", c.ConnectorID,
			map[string]any{"connector_id": c.ConnectorID, "family": c.Family, "owner": c.Owner}))
	if err != nil {
		return nil, false, err
	}
	return &c, created, nil
}

// DeprecateConnector ends new use of a connector from a given date. Existing pinned connections keep working
// until the date (§6.4: supported-until plus an inventory of pinned connections).
func (s *Service) DeprecateConnector(ctx context.Context, tenant string, a Actor, id string, expectedVersion int64, supportedUntil time.Time, reason string) (*Connector, error) {
	if err := platform(tenant); err != nil {
		return nil, err
	}
	if !supportedUntil.After(s.now()) {
		return nil, ErrDeprecateWindow
	}
	if strings.TrimSpace(reason) == "" {
		return nil, &ValidationError{[]Issue{{"reason", "required"}}}
	}
	return s.Repo.MutateConnector(ctx, id, func(c *Connector) (*Event, error) {
		if c.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if c.Lifecycle != ConnectorPublished {
			return nil, ErrTransition
		}
		c.Lifecycle, c.SupportedUntil = ConnectorDeprecated, &supportedUntil
		e := ev(a, "integration.connector.deprecated", "connector", c.ConnectorID,
			map[string]any{"connector_id": c.ConnectorID, "supported_until": supportedUntil.UTC().Format(time.RFC3339), "reason_code": "DEPRECATED"})
		return &e, nil
	})
}

// ── contract versions ────────────────────────────────────────────────────────

// VersionInput is what an author supplies for a draft.
type VersionInput struct {
	Semver            string     `json:"semver"`
	EffectiveFrom     time.Time  `json:"effective_from"`
	SupportedUntil    *time.Time `json:"supported_until,omitempty"`
	MigrationPlanRef  string     `json:"migration_plan_ref,omitempty"`
	ReplayAnalysisRef string     `json:"replay_analysis_ref,omitempty"`
	RollbackRef       string     `json:"rollback_ref,omitempty"`
	Content           Content    `json:"content"`
}

const maxContentBytes = 4 << 20

func (s *Service) build(id, connectorID string, in VersionInput) (*ContractVersion, error) {
	raw, err := json.Marshal(in.Content)
	if err != nil || len(raw) > maxContentBytes {
		return nil, &ValidationError{[]Issue{{"content", "too large or not serializable"}}}
	}
	v := &ContractVersion{
		ContractVersionID: id, ConnectorID: connectorID, Semver: strings.TrimSpace(in.Semver), Status: VersionDraft,
		EffectiveFrom: in.EffectiveFrom.UTC(), SupportedUntil: utcPtr(in.SupportedUntil),
		MigrationPlanRef: in.MigrationPlanRef, ReplayAnalysisRef: in.ReplayAnalysisRef, RollbackRef: in.RollbackRef,
		Content: in.Content,
	}
	if is := ValidateHeader(v); len(is) > 0 {
		return nil, &ValidationError{is}
	}
	// A mapping is stored with its transform hash, so its specification must at least compile now.
	var bad []Issue
	for i, m := range in.Content.Mappings {
		comp, err := mapping.CompileJSON(m.Spec)
		if err != nil {
			bad = append(bad, Issue{fmt.Sprintf("content.mappings[%d].spec", i), "mapping specification is invalid"})
			continue
		}
		v.MappingVersions = append(v.MappingVersions, MappingVersion{
			MappingVersionID: uuid.NewString(), ContractVersionID: id, Key: m.Key, Version: m.Version,
			SourceSchema: m.SourceSchema, CanonicalContract: m.CanonicalContract, Spec: m.Spec,
			TransformHash: comp.Hash(), Status: MappingDraft,
		})
	}
	if len(bad) > 0 {
		return nil, &ValidationError{bad}
	}
	return v, nil
}

// CreateVersion adds a DRAFT contract version to a connector.
func (s *Service) CreateVersion(ctx context.Context, tenant string, a Actor, connectorID string, in VersionInput, idemKey string) (*ContractVersion, bool, error) {
	if err := platform(tenant); err != nil {
		return nil, false, err
	}
	c, err := s.Repo.GetConnector(ctx, connectorID)
	if err != nil {
		return nil, false, err
	}
	if c.Lifecycle == ConnectorRetired {
		return nil, false, ErrTransition
	}
	v, err := s.build(uuid.NewString(), c.ConnectorID, in)
	if err != nil {
		return nil, false, err
	}
	now := s.now()
	v.RowVersion, v.CreatedBy, v.CreatedAt, v.UpdatedAt = 1, a.PrincipalID, now, now
	reqHash := hashOf(connectorID, v.Semver, in.EffectiveFrom.UTC().Format(time.RFC3339Nano), mustJSON(in.Content))
	created, err := s.Repo.CreateVersion(ctx, v, idemKey, reqHash,
		ev(a, "integration.contract.draft-created", "contract-version", v.ContractVersionID,
			map[string]any{"connector_id": c.ConnectorID, "contract_version_id": v.ContractVersionID, "semver": v.Semver}))
	if err != nil {
		return nil, false, err
	}
	if !created {
		// an idempotent replay returns the original draft as it is now
		orig, err := s.Repo.GetVersion(ctx, v.ContractVersionID)
		if err != nil {
			return nil, false, err
		}
		return orig, false, nil
	}
	return v, true, nil
}

func mustJSON(v any) string {
	b, _ := mapping.CanonicalJSON(v)
	return string(b)
}

// ReplaceDraft rewrites a draft. Nothing past DRAFT can be rewritten: a correction is a new version (I-03).
func (s *Service) ReplaceDraft(ctx context.Context, tenant string, a Actor, id string, expectedVersion int64, in VersionInput) (*ContractVersion, error) {
	if err := platform(tenant); err != nil {
		return nil, err
	}
	cur, err := s.Repo.GetVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != VersionDraft {
		return nil, ErrNotDraft
	}
	v, err := s.build(id, cur.ConnectorID, in)
	if err != nil {
		return nil, err
	}
	v.CreatedBy, v.CreatedAt = cur.CreatedBy, cur.CreatedAt
	v.UpdatedAt = s.now()
	return s.Repo.ReplaceDraft(ctx, v, expectedVersion,
		ev(a, "integration.contract.draft-updated", "contract-version", id,
			map[string]any{"connector_id": cur.ConnectorID, "contract_version_id": id, "semver": v.Semver}))
}

// ValidateResult is a validation run and the version as it stands after it.
type ValidateResult struct {
	Version *ContractVersion `json:"version"`
	Run     *ValidationRun   `json:"run"`
}

// Validate runs the schema, security, privacy and compatibility checks of §6.3. The run is recorded whether it
// passes or not. A pass moves DRAFT to VALIDATED and pins the content hash that publication later checks.
func (s *Service) Validate(ctx context.Context, tenant string, a Actor, id string, expectedVersion int64) (*ValidateResult, error) {
	if err := platform(tenant); err != nil {
		return nil, err
	}
	var run *ValidationRun
	v, err := s.Repo.MutateVersion(ctx, id, func(tx *VersionTx) error {
		if tx.V.RowVersion != expectedVersion {
			return ErrVersionConflict
		}
		if tx.V.Status != VersionDraft {
			return ErrNotDraft
		}
		issues := append(ValidateHeader(tx.V), ValidateContent(&tx.V.Content, s.limits())...)
		report := Classify(tx.Baseline, tx.V)
		issues = append(issues, report.Enforce(tx.Baseline, tx.V)...)
		issues = append(issues, schemaReuseIssues(tx.Baseline, tx.V)...)
		if tx.V.RollbackRef == "" {
			issues = append(issues, Issue{"rollback_ref", "a rollback or supersession path is defined before publication"})
		}
		hash, herr := ContentHash(tx.V)
		if herr != nil {
			return herr
		}
		now := s.now()
		run = &ValidationRun{RunID: uuid.NewString(), ContentHash: hash, Issues: issues, Report: report, RunBy: a.PrincipalID, At: now}
		gate := &Gate{GateKey: GateSchemaCompat, EvidenceRef: "validation-run:" + run.RunID, AttestedBy: SystemAttestor, At: now}
		detail, _ := json.Marshal(map[string]string{"content_hash": hash, "run_id": run.RunID})
		gate.Detail = detail
		tx.NewRun, tx.NewGate = run, gate
		if len(issues) > 0 {
			run.Outcome, gate.Outcome = GateFailed, GateFailed
			tx.Events = append(tx.Events, ev(a, "integration.contract.validation-failed", "contract-version", id,
				map[string]any{"connector_id": tx.V.ConnectorID, "contract_version_id": id, "issue_count": len(issues)}))
			return nil // recorded; the version does not move
		}
		run.Outcome, gate.Outcome = GatePassed, GatePassed
		tx.V.Status, tx.V.ContentHash, tx.V.ValidatedAt = VersionValidated, hash, &now
		tx.V.CompatClass, tx.V.CompatReport = report.Class, report
		tx.V.BaselineVersionID = report.BaselineVersionID
		tx.Transition = &Transition{From: VersionDraft, To: VersionValidated, Actor: a.PrincipalID, At: now}
		tx.Events = append(tx.Events, ev(a, "integration.contract.validated", "contract-version", id,
			map[string]any{"connector_id": tx.V.ConnectorID, "contract_version_id": id, "compat_class": report.Class, "content_hash": hash}))
		return nil
	})
	if err != nil {
		return nil, err
	}
	res := &ValidateResult{Version: v, Run: run}
	if run != nil && run.Outcome == GateFailed {
		return res, &ValidationError{Issues: run.Issues}
	}
	return res, nil
}

// schemaReuseIssues enforces "schema version exact": a reference that existed in the baseline means the same
// thing in every later contract. A changed schema takes a new version number.
func schemaReuseIssues(baseline, v *ContractVersion) []Issue {
	if baseline == nil {
		return nil
	}
	old := map[string]string{}
	for i := range baseline.Content.Schemas {
		s := &baseline.Content.Schemas[i]
		old[s.Ref] = SchemaHash(s)
	}
	var is []Issue
	for i := range v.Content.Schemas {
		s := &v.Content.Schemas[i]
		if h, ok := old[s.Ref]; ok && h != SchemaHash(s) {
			is = append(is, Issue{fmt.Sprintf("schemas[%d].schema", i), "this reference already names a different schema; a changed schema takes a new version"})
		}
	}
	return is
}

// SubmitReview moves a validated version to REVIEW.
func (s *Service) SubmitReview(ctx context.Context, tenant string, a Actor, id string, expectedVersion int64) (*ContractVersion, error) {
	if err := platform(tenant); err != nil {
		return nil, err
	}
	return s.Repo.MutateVersion(ctx, id, func(tx *VersionTx) error {
		if tx.V.RowVersion != expectedVersion {
			return ErrVersionConflict
		}
		if tx.V.Status != VersionValidated {
			return ErrNotValidated
		}
		now := s.now()
		tx.V.Status = VersionReview
		tx.Transition = &Transition{From: VersionValidated, To: VersionReview, Actor: a.PrincipalID, At: now}
		if tx.Connector.Lifecycle == ConnectorDraft {
			tx.ConnectorLifecycle = ConnectorReview
		}
		tx.Events = append(tx.Events, ev(a, "integration.contract.submitted", "contract-version", id,
			map[string]any{"connector_id": tx.V.ConnectorID, "contract_version_id": id}))
		return nil
	})
}

// AttestInput is one attestation.
type AttestInput struct {
	Gate        string          `json:"gate"`
	Outcome     string          `json:"outcome"`
	EvidenceRef string          `json:"evidence_ref"`
	Detail      json.RawMessage `json:"detail,omitempty"`
}

type negativeTestDetail struct {
	TestPackVersion string   `json:"test_pack_version"`
	Passed          []string `json:"passed"`
}

// Attest records a gate attestation by someone other than the author. A failed security, privacy or domain
// attestation blocks the version (Figure 4); the others leave it in review to be re-attested.
func (s *Service) Attest(ctx context.Context, tenant string, a Actor, id string, in AttestInput) (*ContractVersion, error) {
	if err := platform(tenant); err != nil {
		return nil, err
	}
	var is []Issue
	if in.Gate == GateSchemaCompat || !contains(RequiredGates, in.Gate) {
		is = append(is, Issue{"gate", "a human-attested gate: SECURITY, PRIVACY, NEGATIVE_TESTS, DOMAIN_ACCEPTANCE or ROLLBACK"})
	}
	if in.Outcome != GatePassed && in.Outcome != GateFailed {
		is = append(is, Issue{"outcome", "PASSED or FAILED"})
	}
	if !refRE.MatchString(in.EvidenceRef) {
		is = append(is, Issue{"evidence_ref", "a reference to the review record is required"})
	}
	if len(is) > 0 {
		return nil, &ValidationError{is}
	}
	return s.Repo.MutateVersion(ctx, id, func(tx *VersionTx) error {
		if tx.V.Status != VersionReview {
			return ErrTransition
		}
		if a.PrincipalID == tx.V.CreatedBy {
			return ErrSelfAttest
		}
		now := s.now()
		if in.Outcome == GatePassed {
			switch in.Gate {
			case GateNegativeTests:
				var d negativeTestDetail
				if err := strictDecode(in.Detail, &d); err != nil || d.TestPackVersion == "" {
					return &ValidationError{[]Issue{{"detail", "test_pack_version and the passed scenario ids"}}}
				}
				have := map[string]bool{}
				for _, p := range d.Passed {
					have[p] = true
				}
				cert := certificationOf(&tx.V.Content)
				if cert == nil || d.TestPackVersion != cert.TestPackVersion {
					return &ValidationError{[]Issue{{"detail.test_pack_version", "must be the certification profile's test pack"}}}
				}
				for _, need := range cert.RequiredNegativeTests {
					if !have[need] {
						return &ValidationError{[]Issue{{"detail.passed", need + " has not passed"}}}
					}
				}
			case GateRollback:
				if tx.V.RollbackRef == "" {
					return &ValidationError{[]Issue{{"rollback_ref", "the version defines no rollback or supersession path"}}}
				}
			}
		}
		tx.NewGate = &Gate{GateKey: in.Gate, Outcome: in.Outcome, EvidenceRef: in.EvidenceRef, Detail: in.Detail, AttestedBy: a.PrincipalID, At: now}
		tx.Events = append(tx.Events, ev(a, "integration.contract.gate-attested", "contract-version", id,
			map[string]any{"connector_id": tx.V.ConnectorID, "contract_version_id": id, "gate": in.Gate, "outcome": in.Outcome}))
		if in.Outcome == GateFailed && (in.Gate == GateSecurity || in.Gate == GatePrivacy || in.Gate == GateDomainAcceptance) {
			tx.V.Status, tx.V.BlockedReason = VersionBlocked, in.Gate+" attestation failed"
			tx.Transition = &Transition{From: VersionReview, To: VersionBlocked, Actor: a.PrincipalID, Reason: tx.V.BlockedReason, At: now}
			tx.Events = append(tx.Events, ev(a, "integration.contract.blocked", "contract-version", id,
				map[string]any{"connector_id": tx.V.ConnectorID, "contract_version_id": id, "reason_code": in.Gate}))
		}
		return nil
	})
}

func certificationOf(c *Content) *certSpec {
	for _, p := range c.Profiles {
		if p.Kind == ProfileCertification {
			var cs certSpec
			if json.Unmarshal(p.Spec, &cs) == nil {
				return &cs
			}
		}
	}
	return nil
}

// Block stops a version that fails review for a reason no gate captures.
func (s *Service) Block(ctx context.Context, tenant string, a Actor, id string, expectedVersion int64, reason string) (*ContractVersion, error) {
	if err := platform(tenant); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 255 {
		return nil, &ValidationError{[]Issue{{"reason", "required, at most 255 characters"}}}
	}
	return s.Repo.MutateVersion(ctx, id, func(tx *VersionTx) error {
		if tx.V.RowVersion != expectedVersion {
			return ErrVersionConflict
		}
		if tx.V.Status != VersionValidated && tx.V.Status != VersionReview {
			return ErrTransition
		}
		from := tx.V.Status
		tx.V.Status, tx.V.BlockedReason = VersionBlocked, reason
		tx.Transition = &Transition{From: from, To: VersionBlocked, Actor: a.PrincipalID, Reason: reason, At: s.now()}
		tx.Events = append(tx.Events, ev(a, "integration.contract.blocked", "contract-version", id,
			map[string]any{"connector_id": tx.V.ConnectorID, "contract_version_id": id, "reason_code": "MANUAL"}))
		return nil
	})
}

// Publish makes a reviewed version PUBLISHED, immutable and the connector's current contract. Existing
// connections stay pinned to the version they accepted (I-04); the version it supersedes is not retired.
func (s *Service) Publish(ctx context.Context, tenant string, a Actor, id string, expectedVersion int64) (*ContractVersion, error) {
	if err := platform(tenant); err != nil {
		return nil, err
	}
	return s.Repo.MutateVersion(ctx, id, func(tx *VersionTx) error {
		v := tx.V
		if v.RowVersion != expectedVersion {
			return ErrVersionConflict
		}
		if v.Status != VersionReview {
			return ErrTransition
		}
		if a.PrincipalID == v.CreatedBy {
			return ErrSelfPublish
		}
		// the content is exactly what was validated and reviewed
		hash, err := ContentHash(v)
		if err != nil {
			return err
		}
		if v.ContentHash == "" || hash != v.ContentHash {
			return ErrStaleValidation
		}
		// nothing was published since validation moved the baseline this version was compared with
		curBase := ""
		if tx.Baseline != nil {
			curBase = tx.Baseline.ContractVersionID
		}
		if curBase != v.BaselineVersionID {
			return ErrBaselineMoved
		}
		if tx.Baseline != nil && semverCompare(v.Semver, tx.Baseline.Semver) <= 0 {
			return ErrVersionOrder
		}
		// every gate passed on this content
		latest := map[string]Gate{}
		for _, g := range tx.Gates {
			latest[g.GateKey] = g
		}
		for _, k := range RequiredGates {
			g, ok := latest[k]
			if !ok || g.Outcome != GatePassed {
				return ErrGatesIncomplete
			}
			if k == GateSchemaCompat {
				var d struct {
					ContentHash string `json:"content_hash"`
				}
				if json.Unmarshal(g.Detail, &d) != nil || d.ContentHash != v.ContentHash {
					return ErrStaleValidation
				}
			}
		}
		now := s.now()
		v.Status, v.PublishedBy, v.PublishedAt = VersionPublished, a.PrincipalID, &now
		tx.Transition = &Transition{From: VersionReview, To: VersionPublished, Actor: a.PrincipalID, At: now}
		tx.ConnectorLifecycle = ConnectorPublished
		ops := make([]string, 0, len(v.Content.Operations))
		for _, o := range v.Content.Operations {
			ops = append(ops, o.ID)
		}
		tx.Events = append(tx.Events, ev(a, "integration.contract.published", "contract-version", id, map[string]any{
			"connector_id": v.ConnectorID, "contract_version_id": id, "semver": v.Semver, "capabilities": ops,
			"effective_from": v.EffectiveFrom.Format(time.RFC3339), "owner": tx.Connector.Owner, "compat_class": v.CompatClass,
		}))
		if tx.Baseline != nil {
			tx.Supersede = tx.Baseline.ContractVersionID
			tx.Events = append(tx.Events, ev(a, "integration.contract.superseded", "contract-version", tx.Baseline.ContractVersionID,
				map[string]any{"connector_id": v.ConnectorID, "contract_version_id": tx.Baseline.ContractVersionID, "superseded_by": id}))
		}
		return nil
	})
}

// ── reads ────────────────────────────────────────────────────────────────────

// VersionCompatibility is one row of the compatibility view.
type VersionCompatibility struct {
	ContractVersionID string     `json:"contract_version_id"`
	Semver            string     `json:"semver"`
	Status            string     `json:"status"`
	EffectiveFrom     time.Time  `json:"effective_from"`
	SupportedUntil    *time.Time `json:"supported_until,omitempty"`
	CompatClass       string     `json:"compat_class,omitempty"`
	// Supported says whether a connection may be (or stay) pinned to this version right now.
	Supported bool `json:"supported"`
	// MigrationRequired says the version that replaced it changed something a pinned connection must be moved
	// across deliberately.
	MigrationRequired bool `json:"migration_required"`
}

// Compatibility is the §6.5 "supported / pinned / migration status" view of a connector.
type Compatibility struct {
	Connector *Connector             `json:"connector"`
	Current   string                 `json:"current_contract_version_id,omitempty"`
	Versions  []VersionCompatibility `json:"versions"`
}

// Compatibility reports what versions of a connector exist and which may still be used.
func (s *Service) Compatibility(ctx context.Context, connectorID string) (*Compatibility, error) {
	c, err := s.Repo.GetConnector(ctx, connectorID)
	if err != nil {
		return nil, err
	}
	vs, err := s.Repo.ListVersions(ctx, connectorID)
	if err != nil {
		return nil, err
	}
	out := &Compatibility{Connector: c, Versions: make([]VersionCompatibility, 0, len(vs))}
	now := s.now()
	for i, v := range vs { // ascending semver
		row := VersionCompatibility{ContractVersionID: v.ContractVersionID, Semver: v.Semver, Status: v.Status,
			EffectiveFrom: v.EffectiveFrom, SupportedUntil: v.SupportedUntil, CompatClass: v.CompatClass}
		switch v.Status {
		case VersionPublished:
			out.Current = v.ContractVersionID
			row.Supported = v.SupportedUntil == nil || v.SupportedUntil.After(now)
		case VersionSuperseded:
			row.Supported = v.SupportedUntil == nil || v.SupportedUntil.After(now)
			for _, next := range vs[i+1:] {
				if next.Status == VersionPublished || next.Status == VersionSuperseded {
					if cl := next.CompatClass; cl != "" && classRank[cl] >= classRank[ClassLifecycle] {
						row.MigrationRequired = true
					}
				}
			}
		}
		out.Versions = append(out.Versions, row)
	}
	return out, nil
}

// SimulationSample is one sample for a dry run.
type SimulationSample struct {
	Source json.RawMessage `json:"source"`
	// ExpectedCanonicalHash, when set, turns the sample into a golden check.
	ExpectedCanonicalHash string `json:"expected_canonical_hash,omitempty"`
}

// SimulationResult is the outcome for one sample.
type SimulationResult struct {
	*mapping.Result
	GoldenMatch *bool `json:"golden_match,omitempty"`
}

const maxSimulationSamples = 20

// Simulate replays sample documents through a stored mapping without writing anything: no row, no event, no
// state. It is how a mapping is reviewed before the contract that carries it is published.
func (s *Service) Simulate(ctx context.Context, mappingVersionID string, samples []SimulationSample) ([]SimulationResult, error) {
	if len(samples) == 0 || len(samples) > maxSimulationSamples {
		return nil, &ValidationError{[]Issue{{"samples", fmt.Sprintf("between 1 and %d samples", maxSimulationSamples)}}}
	}
	m, err := s.Repo.GetMapping(ctx, mappingVersionID)
	if err != nil {
		return nil, err
	}
	comp, err := mapping.CompileJSON(m.Spec)
	if err != nil {
		return nil, err
	}
	out := make([]SimulationResult, 0, len(samples))
	for _, sm := range samples {
		r := comp.Evaluate(sm.Source, m.MappingVersionID, 0)
		sr := SimulationResult{Result: r}
		if sm.ExpectedCanonicalHash != "" {
			ok := r.OK() && r.CanonicalHash == sm.ExpectedCanonicalHash
			sr.GoldenMatch = &ok
		}
		out = append(out, sr)
	}
	return out, nil
}

// IsNotFound reports whether err is one of the registry's not-found errors.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrConnectorNotFound) || errors.Is(err, ErrVersionNotFound) || errors.Is(err, ErrMappingNotFound)
}
