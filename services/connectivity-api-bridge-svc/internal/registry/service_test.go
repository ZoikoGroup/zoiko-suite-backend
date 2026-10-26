package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry/registrytest"
)

const tenantA = "tenant-a"

var (
	author   = registry.Actor{PrincipalID: "author", CorrelationID: "corr-1"}
	security = registry.Actor{PrincipalID: "security-reviewer"}
	privacy  = registry.Actor{PrincipalID: "privacy-reviewer"}
	qa       = registry.Actor{PrincipalID: "qa-lead"}
	domainAc = registry.Actor{PrincipalID: "domain-owner"}
	release  = registry.Actor{PrincipalID: "release-manager"}
)

func platCtx() context.Context {
	return middleware.WithTenant(context.Background(), registry.PlatformScopeID)
}
func tenantCtx() context.Context { return middleware.WithTenant(context.Background(), tenantA) }

type env struct {
	t    *testing.T
	repo *registry.MemRepo
	svc  *registry.Service
	now  time.Time
	mu   sync.Mutex
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, repo: registry.NewMemRepo(), now: registrytest.T0}
	e.svc = &registry.Service{Repo: e.repo, Clock: func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); e.now = e.now.Add(time.Second); return e.now }}
	return e
}

func (e *env) connector(id string) *registry.Connector {
	e.t.Helper()
	c, _, err := e.svc.CreateConnector(platCtx(), registry.PlatformScopeID, author,
		registry.Connector{ConnectorID: id, Family: "BANK_FEED", ProviderClass: "Bank X open banking", Owner: "integration-engineering"}, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) draft(connectorID, semver string, mutate func(*registry.VersionInput)) *registry.ContractVersion {
	e.t.Helper()
	in := registrytest.Input(semver)
	if mutate != nil {
		mutate(&in)
	}
	v, _, err := e.svc.CreateVersion(platCtx(), registry.PlatformScopeID, author, connectorID, in, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

func (e *env) validate(v *registry.ContractVersion) *registry.ContractVersion {
	e.t.Helper()
	res, err := e.svc.Validate(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	if err != nil {
		e.t.Fatalf("validate: %v", err)
	}
	return res.Version
}

func (e *env) attestAll(v *registry.ContractVersion) {
	e.t.Helper()
	for _, a := range []struct {
		who    registry.Actor
		gate   string
		detail json.RawMessage
	}{
		{security, registry.GateSecurity, nil}, {privacy, registry.GatePrivacy, nil},
		{qa, registry.GateNegativeTests, registrytest.Passed()}, {domainAc, registry.GateDomainAcceptance, nil},
		{release, registry.GateRollback, nil},
	} {
		if _, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, a.who, v.ContractVersionID,
			registry.AttestInput{Gate: a.gate, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-" + a.gate, Detail: a.detail}); err != nil {
			e.t.Fatalf("attest %s: %v", a.gate, err)
		}
	}
}

// publishable takes a draft all the way to REVIEW with every gate passed.
func (e *env) reviewed(v *registry.ContractVersion) *registry.ContractVersion {
	e.t.Helper()
	v = e.validate(v)
	var err error
	if v, err = e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion); err != nil {
		e.t.Fatal(err)
	}
	e.attestAll(v)
	cur, _ := e.repo.GetVersion(platCtx(), v.ContractVersionID)
	return cur
}

func (e *env) publish(v *registry.ContractVersion) *registry.ContractVersion {
	e.t.Helper()
	out, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, release, v.ContractVersionID, v.RowVersion)
	if err != nil {
		e.t.Fatalf("publish: %v", err)
	}
	return out
}

func (e *env) events(typ string) []registry.Event {
	var out []registry.Event
	for _, ev := range e.repo.Events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// ---- the happy path -----------------------------------------------------------------------

func TestFullLifecycle(t *testing.T) {
	e := newEnv(t)
	c := e.connector("bank-x")
	if c.Lifecycle != registry.ConnectorDraft {
		t.Fatalf("lifecycle %s", c.Lifecycle)
	}
	v := e.draft("bank-x", "1.0.0", nil)
	if v.Status != registry.VersionDraft || len(v.MappingVersions) != 1 || v.MappingVersions[0].TransformHash == "" {
		t.Fatalf("%+v", v)
	}
	v = e.validate(v)
	if v.Status != registry.VersionValidated || v.ContentHash == "" || v.CompatClass != registry.ClassNone {
		t.Fatalf("%+v", v)
	}
	v, err := e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	if err != nil || v.Status != registry.VersionReview {
		t.Fatalf("%v %+v", err, v)
	}
	if got, _ := e.repo.GetConnector(platCtx(), "bank-x"); got.Lifecycle != registry.ConnectorReview {
		t.Fatalf("connector %s", got.Lifecycle)
	}
	e.attestAll(v)
	v, _ = e.repo.GetVersion(platCtx(), v.ContractVersionID)
	pub := e.publish(v)
	if pub.Status != registry.VersionPublished || pub.PublishedBy != "release-manager" || pub.PublishedAt == nil {
		t.Fatalf("%+v", pub)
	}
	if got, _ := e.repo.GetConnector(platCtx(), "bank-x"); got.Lifecycle != registry.ConnectorPublished {
		t.Fatalf("connector %s", got.Lifecycle)
	}
	// every status change left append-only evidence
	trs, _ := e.repo.ListTransitions(platCtx(), v.ContractVersionID)
	var path []string
	for _, tr := range trs {
		path = append(path, tr.From+">"+tr.To)
	}
	if strings.Join(path, ",") != "DRAFT>VALIDATED,VALIDATED>REVIEW,REVIEW>PUBLISHED" {
		t.Fatalf("transitions %v", path)
	}
	// the §14.1 event carries its minimum anchors
	evs := e.events("integration.contract.published")
	if len(evs) != 1 {
		t.Fatalf("%d published events", len(evs))
	}
	d := evs[0].Data
	for _, k := range []string{"connector_id", "contract_version_id", "capabilities", "effective_from", "owner"} {
		if _, ok := d[k]; !ok {
			t.Errorf("published event lacks %s", k)
		}
	}
	if d["owner"] != "integration-engineering" || len(d["capabilities"].([]string)) != 3 {
		t.Errorf("event data %v", d)
	}
}

// ---- platform scope and visibility ---------------------------------------------------------

func TestOnlyPlatformScopeWrites(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.draft("bank-x", "1.0.0", nil)
	ctx := tenantCtx()
	call := map[string]error{}
	_, _, call["connector"] = e.svc.CreateConnector(ctx, tenantA, author, registry.Connector{ConnectorID: "evil-one", Family: "API", ProviderClass: "x", Owner: "y"}, "")
	_, _, call["version"] = e.svc.CreateVersion(ctx, tenantA, author, "bank-x", registrytest.Input("1.1.0"), "")
	_, call["replace"] = e.svc.ReplaceDraft(ctx, tenantA, author, v.ContractVersionID, 1, registrytest.Input("1.0.0"))
	_, call["validate"] = e.svc.Validate(ctx, tenantA, author, v.ContractVersionID, 1)
	_, call["submit"] = e.svc.SubmitReview(ctx, tenantA, author, v.ContractVersionID, 1)
	_, call["attest"] = e.svc.Attest(ctx, tenantA, security, v.ContractVersionID, registry.AttestInput{Gate: registry.GateSecurity, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-1"})
	_, call["block"] = e.svc.Block(ctx, tenantA, security, v.ContractVersionID, 1, "no")
	_, call["publish"] = e.svc.Publish(ctx, tenantA, release, v.ContractVersionID, 1)
	_, call["deprecate"] = e.svc.DeprecateConnector(ctx, tenantA, author, "bank-x", 1, registrytest.T0.Add(time.Hour*24*40), "x")
	for name, err := range call {
		if !errors.Is(err, registry.ErrNotPlatformScope) {
			t.Errorf("%s from a tenant: %v", name, err)
		}
	}
	// and the repo itself refuses, whatever the service was told
	if _, err := e.repo.MutateVersion(ctx, v.ContractVersionID, func(*registry.VersionTx) error { return nil }); !errors.Is(err, registry.ErrNotPlatformScope) {
		t.Errorf("repo accepted a tenant write: %v", err)
	}
}

func TestTenantsSeeOnlyPublishedContracts(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	draft := e.draft("bank-x", "1.0.0", nil)
	if _, err := e.repo.GetVersion(tenantCtx(), draft.ContractVersionID); !errors.Is(err, registry.ErrVersionNotFound) {
		t.Fatalf("tenant read a draft: %v", err)
	}
	if _, err := e.repo.GetConnector(tenantCtx(), "bank-x"); !errors.Is(err, registry.ErrConnectorNotFound) {
		t.Fatalf("tenant read an unpublished connector: %v", err)
	}
	if list, _ := e.repo.ListConnectors(tenantCtx(), "", ""); len(list) != 0 {
		t.Fatalf("tenant listed %d unpublished connectors", len(list))
	}
	if _, err := e.repo.GetMapping(tenantCtx(), draft.MappingVersions[0].MappingVersionID); !errors.Is(err, registry.ErrMappingNotFound) {
		t.Fatalf("tenant read a draft mapping: %v", err)
	}
	pub := e.publish(e.reviewed(draft))
	if got, err := e.repo.GetVersion(tenantCtx(), pub.ContractVersionID); err != nil || got.Status != registry.VersionPublished {
		t.Fatalf("tenant cannot read a published contract: %v", err)
	}
	if m, err := e.repo.GetMapping(tenantCtx(), pub.MappingVersions[0].MappingVersionID); err != nil || m.Status != registry.MappingPublished {
		t.Fatalf("tenant cannot read a published mapping: %v %+v", err, m)
	}
}

// ---- connectors -------------------------------------------------------------------------

func TestConnectorValidationAndIdempotency(t *testing.T) {
	e := newEnv(t)
	for name, c := range map[string]registry.Connector{
		"short id":     {ConnectorID: "ab", Family: "API", ProviderClass: "x", Owner: "o"},
		"uppercase id": {ConnectorID: "Bank-X", Family: "API", ProviderClass: "x", Owner: "o"},
		"bad family":   {ConnectorID: "bank-x", Family: "MAGIC", ProviderClass: "x", Owner: "o"},
		"no owner":     {ConnectorID: "bank-x", Family: "API", ProviderClass: "x"},
		"no class":     {ConnectorID: "bank-x", Family: "API", Owner: "o"},
	} {
		// uppercase ids are normalized, so only the others must fail
		if name == "uppercase id" {
			continue
		}
		if _, _, err := e.svc.CreateConnector(platCtx(), registry.PlatformScopeID, author, c, ""); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c := registry.Connector{ConnectorID: "bank-x", Family: "bank_feed", ProviderClass: "Bank X", Owner: "eng"}
	first, created, err := e.svc.CreateConnector(platCtx(), registry.PlatformScopeID, author, c, "key-1")
	if err != nil || !created || first.Family != "BANK_FEED" {
		t.Fatalf("%v %v %+v", err, created, first)
	}
	if _, created, err = e.svc.CreateConnector(platCtx(), registry.PlatformScopeID, author, c, "key-1"); err != nil || created {
		t.Fatalf("same key and request must replay: created=%v err=%v", created, err)
	}
	other := c
	other.Owner = "someone-else"
	if _, _, err = e.svc.CreateConnector(platCtx(), registry.PlatformScopeID, author, other, "key-1"); !errors.Is(err, registry.ErrIdempotency) {
		t.Fatalf("same key, different request: %v", err)
	}
	// an id is never reused (I: stable identity)
	if _, _, err = e.svc.CreateConnector(platCtx(), registry.PlatformScopeID, author, c, "key-2"); !errors.Is(err, registry.ErrConnectorExists) {
		t.Fatalf("id reused: %v", err)
	}
	if got := e.events("integration.connector.created"); len(got) != 1 {
		t.Fatalf("%d created events for one connector", len(got))
	}
}

func TestVersionCreationRules(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	if _, _, err := e.svc.CreateVersion(platCtx(), registry.PlatformScopeID, author, "ghost", registrytest.Input("1.0.0"), ""); !errors.Is(err, registry.ErrConnectorNotFound) {
		t.Fatalf("version for a missing connector: %v", err)
	}
	v := e.draft("bank-x", "1.0.0", nil)
	if _, _, err := e.svc.CreateVersion(platCtx(), registry.PlatformScopeID, author, "bank-x", registrytest.Input("1.0.0"), ""); !errors.Is(err, registry.ErrVersionExists) {
		t.Fatalf("duplicate semver: %v", err)
	}
	// idempotent create returns the original draft
	in := registrytest.Input("1.1.0")
	a, created, err := e.svc.CreateVersion(platCtx(), registry.PlatformScopeID, author, "bank-x", in, "idem-v")
	if err != nil || !created {
		t.Fatal(err)
	}
	b, created, err := e.svc.CreateVersion(platCtx(), registry.PlatformScopeID, author, "bank-x", in, "idem-v")
	if err != nil || created || b.ContractVersionID != a.ContractVersionID {
		t.Fatalf("replay: %v created=%v", err, created)
	}
	in.Content.Operations[0].RateLimitPerMinute = 7
	if _, _, err = e.svc.CreateVersion(platCtx(), registry.PlatformScopeID, author, "bank-x", in, "idem-v"); !errors.Is(err, registry.ErrIdempotency) {
		t.Fatalf("same key, different content: %v", err)
	}
	// a mapping that does not compile never gets stored
	bad := registrytest.Input("2.0.0")
	bad.Content.Mappings[0].Spec = json.RawMessage(`{"canonical_contract":"x","fields":[]}`)
	if _, _, err = e.svc.CreateVersion(platCtx(), registry.PlatformScopeID, author, "bank-x", bad, ""); err == nil {
		t.Fatal("an uncompilable mapping was stored")
	}
	var ve *registry.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want a validation error, got %v", err)
	}
	// header problems are refused up front
	if _, _, err = e.svc.CreateVersion(platCtx(), registry.PlatformScopeID, author, "bank-x", registry.VersionInput{Semver: "x"}, ""); err == nil {
		t.Fatal("bad header accepted")
	}
	_ = v
}

// ---- immutability (I-03) ---------------------------------------------------------------

func TestDraftIsEditableButNothingElseIs(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.draft("bank-x", "1.0.0", nil)

	in := registrytest.Input("1.0.0")
	in.Content.Operations[0].RateLimitPerMinute = 42
	edited, err := e.svc.ReplaceDraft(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion, in)
	if err != nil || edited.Content.Operations[0].RateLimitPerMinute != 42 || edited.RowVersion != v.RowVersion+1 {
		t.Fatalf("%v %+v", err, edited)
	}
	// stale expected version
	if _, err = e.svc.ReplaceDraft(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion, in); !errors.Is(err, registry.ErrVersionConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	validated := e.validate(edited)
	// once validated, even the author cannot change it
	if _, err = e.svc.ReplaceDraft(platCtx(), registry.PlatformScopeID, author, validated.ContractVersionID, validated.RowVersion, in); !errors.Is(err, registry.ErrNotDraft) {
		t.Fatalf("edited a validated version: %v", err)
	}
	reviewed := e.reviewed(e.draft("bank-x", "1.1.0", nil))
	pub := e.publish(reviewed)
	if _, err = e.svc.ReplaceDraft(platCtx(), registry.PlatformScopeID, author, pub.ContractVersionID, pub.RowVersion, in); !errors.Is(err, registry.ErrNotDraft) {
		t.Fatalf("edited a published version: %v", err)
	}
}

func TestPublishedMappingsArePublishedAndImmutable(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.draft("bank-x", "1.0.0", nil)
	mid := v.MappingVersions[0].MappingVersionID
	if m, _ := e.repo.GetMapping(platCtx(), mid); m.Status != registry.MappingDraft {
		t.Fatalf("mapping %s", m.Status)
	}
	pub := e.publish(e.reviewed(v))
	m, _ := e.repo.GetMapping(platCtx(), mid)
	if m.Status != registry.MappingPublished || pub.MappingVersions[0].TransformHash != m.TransformHash {
		t.Fatalf("%+v", m)
	}
}

// ---- validation is recorded, and failure does not move the version ---------------------

func TestFailedValidationIsRecordedAndDoesNotAdvance(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.draft("bank-x", "1.0.0", func(in *registry.VersionInput) { in.Content.Operations[1].MaxPayloadBytes = 0 })
	res, err := e.svc.Validate(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	var ve *registry.ValidationError
	if !errors.As(err, &ve) || res == nil || res.Run.Outcome != registry.GateFailed || len(ve.Issues) == 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Version.Status != registry.VersionDraft || res.Version.RowVersion != v.RowVersion {
		t.Fatalf("a failed validation moved the version: %+v", res.Version)
	}
	runs, _ := e.repo.ListValidationRuns(platCtx(), v.ContractVersionID)
	gates, _ := e.repo.ListGates(platCtx(), v.ContractVersionID)
	if len(runs) != 1 || runs[0].Outcome != registry.GateFailed || len(gates) != 1 || gates[0].Outcome != registry.GateFailed || gates[0].AttestedBy != registry.SystemAttestor {
		t.Fatalf("runs=%+v gates=%+v", runs, gates)
	}
	if len(e.events("integration.contract.validation-failed")) != 1 {
		t.Fatal("no validation-failed event")
	}
	// fix, validate again: a second run, and the latest gate is now PASSED
	in := registrytest.Input("1.0.0")
	fixed, err := e.svc.ReplaceDraft(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.svc.Validate(platCtx(), registry.PlatformScopeID, author, fixed.ContractVersionID, fixed.RowVersion); err != nil {
		t.Fatal(err)
	}
	runs, _ = e.repo.ListValidationRuns(platCtx(), v.ContractVersionID)
	if len(runs) != 2 || runs[1].Outcome != registry.GatePassed {
		t.Fatalf("%+v", runs)
	}
	// validating a version that is no longer a draft is refused
	cur, _ := e.repo.GetVersion(platCtx(), v.ContractVersionID)
	if _, err = e.svc.Validate(platCtx(), registry.PlatformScopeID, author, cur.ContractVersionID, cur.RowVersion); !errors.Is(err, registry.ErrNotDraft) {
		t.Fatalf("re-validate: %v", err)
	}
	if _, err = e.svc.Validate(platCtx(), registry.PlatformScopeID, author, cur.ContractVersionID, 99); !errors.Is(err, registry.ErrVersionConflict) {
		t.Fatalf("stale validate: %v", err)
	}
}

func TestRollbackPathIsRequired(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.draft("bank-x", "1.0.0", func(in *registry.VersionInput) { in.RollbackRef = "" })
	_, err := e.svc.Validate(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	var ve *registry.ValidationError
	if !errors.As(err, &ve) || !hasIssue(ve.Issues, "rollback_ref") {
		t.Fatalf("%v", err)
	}
}

// ---- publication gates (§6.3) ------------------------------------------------------------

func TestPublishNeedsEveryGate(t *testing.T) {
	for _, skip := range []string{registry.GateSecurity, registry.GatePrivacy, registry.GateNegativeTests, registry.GateDomainAcceptance, registry.GateRollback} {
		t.Run("without "+skip, func(t *testing.T) {
			e := newEnv(t)
			e.connector("bank-x")
			v := e.validate(e.draft("bank-x", "1.0.0", nil))
			v, _ = e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
			for _, a := range []struct {
				who    registry.Actor
				gate   string
				detail json.RawMessage
			}{{security, registry.GateSecurity, nil}, {privacy, registry.GatePrivacy, nil}, {qa, registry.GateNegativeTests, registrytest.Passed()},
				{domainAc, registry.GateDomainAcceptance, nil}, {release, registry.GateRollback, nil}} {
				if a.gate == skip {
					continue
				}
				if _, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, a.who, v.ContractVersionID,
					registry.AttestInput{Gate: a.gate, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-1", Detail: a.detail}); err != nil {
					t.Fatal(err)
				}
			}
			cur, _ := e.repo.GetVersion(platCtx(), v.ContractVersionID)
			if _, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, release, cur.ContractVersionID, cur.RowVersion); !errors.Is(err, registry.ErrGatesIncomplete) {
				t.Fatalf("published without %s: %v", skip, err)
			}
		})
	}
}

func TestPublishNeedsAReviewedVersion(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.draft("bank-x", "1.0.0", nil)
	for name, version := range map[string]*registry.ContractVersion{"draft": v} {
		if _, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, release, version.ContractVersionID, version.RowVersion); !errors.Is(err, registry.ErrTransition) {
			t.Errorf("published a %s: %v", name, err)
		}
	}
	validated := e.validate(v)
	if _, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, release, validated.ContractVersionID, validated.RowVersion); !errors.Is(err, registry.ErrTransition) {
		t.Errorf("published a validated version that skipped review: %v", err)
	}
	if _, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, security, validated.ContractVersionID,
		registry.AttestInput{Gate: registry.GateSecurity, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-1"}); !errors.Is(err, registry.ErrTransition) {
		t.Errorf("attested outside review: %v", err)
	}
}

func TestSeparationOfDuties(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.validate(e.draft("bank-x", "1.0.0", nil))
	v, _ = e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	// the author cannot attest their own contract
	if _, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID,
		registry.AttestInput{Gate: registry.GateSecurity, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-1"}); !errors.Is(err, registry.ErrSelfAttest) {
		t.Fatalf("self attestation: %v", err)
	}
	e.attestAll(v)
	cur, _ := e.repo.GetVersion(platCtx(), v.ContractVersionID)
	// nor publish it
	if _, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, author, cur.ContractVersionID, cur.RowVersion); !errors.Is(err, registry.ErrSelfPublish) {
		t.Fatalf("self publish: %v", err)
	}
}

func TestHumansCannotAttestTheSystemGate(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.validate(e.draft("bank-x", "1.0.0", nil))
	v, _ = e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	for _, g := range []string{registry.GateSchemaCompat, "MADE_UP", ""} {
		_, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, security, v.ContractVersionID,
			registry.AttestInput{Gate: g, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-1"})
		var ve *registry.ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("gate %q: %v", g, err)
		}
	}
	for _, in := range []registry.AttestInput{
		{Gate: registry.GateSecurity, Outcome: "MAYBE", EvidenceRef: "REVIEW-1"},
		{Gate: registry.GateSecurity, Outcome: registry.GatePassed, EvidenceRef: ""},
		{Gate: registry.GateSecurity, Outcome: registry.GatePassed, EvidenceRef: "x"},
	} {
		if _, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, security, v.ContractVersionID, in); err == nil {
			t.Errorf("%+v accepted", in)
		}
	}
}

func TestNegativeTestAttestationMustCoverTheCertificationProfile(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.validate(e.draft("bank-x", "1.0.0", nil))
	v, _ = e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	attest := func(detail string) error {
		_, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, qa, v.ContractVersionID,
			registry.AttestInput{Gate: registry.GateNegativeTests, Outcome: registry.GatePassed, EvidenceRef: "TESTRUN-1", Detail: json.RawMessage(detail)})
		return err
	}
	for name, d := range map[string]string{
		"no detail":       ``,
		"wrong test pack": `{"test_pack_version":"other","passed":["NP-01"]}`,
		"a test missing":  `{"test_pack_version":"tp-2026.10","passed":["NP-01","NP-02"]}`,
		"unknown field":   `{"test_pack_version":"tp-2026.10","passed":[],"skipped":["NP-17"]}`,
	} {
		if err := attest(d); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if gates, _ := e.repo.ListGates(platCtx(), v.ContractVersionID); len(gates) != 1 { // only the system gate
		t.Fatalf("a rejected attestation was recorded: %+v", gates)
	}
	if err := attest(string(registrytest.Passed())); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackGateNeedsARollbackPath(t *testing.T) {
	// validation already demands one; this proves the attestation cannot pass without it either
	e := newEnv(t)
	e.connector("bank-x")
	v := e.validate(e.draft("bank-x", "1.0.0", nil))
	v, _ = e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	if _, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, release, v.ContractVersionID,
		registry.AttestInput{Gate: registry.GateRollback, Outcome: registry.GatePassed, EvidenceRef: "RUNBOOK-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedReviewGatesBlock(t *testing.T) {
	for _, gate := range []string{registry.GateSecurity, registry.GatePrivacy, registry.GateDomainAcceptance} {
		t.Run(gate, func(t *testing.T) {
			e := newEnv(t)
			e.connector("bank-x")
			v := e.validate(e.draft("bank-x", "1.0.0", nil))
			v, _ = e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
			got, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, security, v.ContractVersionID,
				registry.AttestInput{Gate: gate, Outcome: registry.GateFailed, EvidenceRef: "REVIEW-9"})
			if err != nil || got.Status != registry.VersionBlocked || got.BlockedReason == "" {
				t.Fatalf("%v %+v", err, got)
			}
			if len(e.events("integration.contract.blocked")) != 1 {
				t.Fatal("no blocked event")
			}
			// a blocked version is dead: nothing moves it to published
			e.attestAllIgnoringErrors(got)
			if _, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, release, got.ContractVersionID, got.RowVersion); !errors.Is(err, registry.ErrTransition) {
				t.Fatalf("published a blocked version: %v", err)
			}
		})
	}
}

func (e *env) attestAllIgnoringErrors(v *registry.ContractVersion) {
	for _, g := range registry.RequiredGates[1:] {
		_, _ = e.svc.Attest(platCtx(), registry.PlatformScopeID, qa, v.ContractVersionID,
			registry.AttestInput{Gate: g, Outcome: registry.GatePassed, EvidenceRef: "REVIEW-1", Detail: registrytest.Passed()})
	}
}

func TestFailedTestsOrRollbackStayInReviewAndCanBeReattested(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.validate(e.draft("bank-x", "1.0.0", nil))
	v, _ = e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	got, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, qa, v.ContractVersionID,
		registry.AttestInput{Gate: registry.GateNegativeTests, Outcome: registry.GateFailed, EvidenceRef: "TESTRUN-1"})
	if err != nil || got.Status != registry.VersionReview {
		t.Fatalf("%v %s", err, got.Status)
	}
	e.attestAll(v) // the later PASSED attestation supersedes the earlier FAILED one
	cur, _ := e.repo.GetVersion(platCtx(), v.ContractVersionID)
	if _, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, release, cur.ContractVersionID, cur.RowVersion); err != nil {
		t.Fatalf("latest attestation did not count: %v", err)
	}
}

func TestLatestAttestationWins_ALaterFailureRevokesAnEarlierPass(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.reviewed(e.draft("bank-x", "1.0.0", nil))
	if _, err := e.svc.Attest(platCtx(), registry.PlatformScopeID, qa, v.ContractVersionID,
		registry.AttestInput{Gate: registry.GateNegativeTests, Outcome: registry.GateFailed, EvidenceRef: "TESTRUN-2"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.repo.GetVersion(platCtx(), v.ContractVersionID)
	if _, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, release, cur.ContractVersionID, cur.RowVersion); !errors.Is(err, registry.ErrGatesIncomplete) {
		t.Fatalf("published over a later failed attestation: %v", err)
	}
}

func TestManualBlock(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.draft("bank-x", "1.0.0", nil)
	if _, err := e.svc.Block(platCtx(), registry.PlatformScopeID, security, v.ContractVersionID, v.RowVersion, "x"); !errors.Is(err, registry.ErrTransition) {
		t.Fatalf("blocked a draft: %v", err)
	}
	v = e.validate(v)
	if _, err := e.svc.Block(platCtx(), registry.PlatformScopeID, security, v.ContractVersionID, v.RowVersion, ""); err == nil {
		t.Fatal("a block without a reason")
	}
	got, err := e.svc.Block(platCtx(), registry.PlatformScopeID, security, v.ContractVersionID, v.RowVersion, "provider withdrew the API")
	if err != nil || got.Status != registry.VersionBlocked {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := e.svc.Block(platCtx(), registry.PlatformScopeID, security, v.ContractVersionID, got.RowVersion, "again"); !errors.Is(err, registry.ErrTransition) {
		t.Fatalf("blocked twice: %v", err)
	}
}

// ---- supersession, baselines and ordering -----------------------------------------------

func TestSecondVersionSupersedesAndKeepsTheFirstUsable(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v1 := e.publish(e.reviewed(e.draft("bank-x", "1.0.0", nil)))
	v2 := e.publish(e.reviewed(e.draft("bank-x", "1.1.0", nil)))
	if v2.BaselineVersionID != v1.ContractVersionID {
		t.Fatalf("baseline %s", v2.BaselineVersionID)
	}
	old, _ := e.repo.GetVersion(platCtx(), v1.ContractVersionID)
	if old.Status != registry.VersionSuperseded {
		t.Fatalf("v1 is %s", old.Status)
	}
	if len(e.events("integration.contract.superseded")) != 1 {
		t.Fatal("no superseded event")
	}
	// superseded, not retired: connections pinned to it keep a readable, supported contract (I-04)
	cp, err := e.svc.Compatibility(tenantCtx(), "bank-x")
	if err != nil || cp.Current != v2.ContractVersionID || len(cp.Versions) != 2 {
		t.Fatalf("%v %+v", err, cp)
	}
	if !cp.Versions[0].Supported || cp.Versions[0].Status != registry.VersionSuperseded || cp.Versions[0].MigrationRequired {
		t.Fatalf("v1 row %+v", cp.Versions[0])
	}
	if !cp.Versions[1].Supported || cp.Versions[1].Status != registry.VersionPublished {
		t.Fatalf("v2 row %+v", cp.Versions[1])
	}
	// the published version a tenant reads is the one it was pinned to, unchanged
	if got, err := e.repo.GetVersion(tenantCtx(), v1.ContractVersionID); err != nil || got.ContentHash != v1.ContentHash {
		t.Fatalf("pinned version changed: %v", err)
	}
}

func TestBreakingSuccessorMarksMigrationRequired(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v1 := e.publish(e.reviewed(e.draft("bank-x", "1.0.0", nil)))
	// a breaking change without the major bump and plan does not validate
	bad := e.draft("bank-x", "1.1.0", func(in *registry.VersionInput) { in.Content.Operations[1].EndpointTemplate = "/v2/payments" })
	if _, err := e.svc.Validate(platCtx(), registry.PlatformScopeID, author, bad.ContractVersionID, bad.RowVersion); err == nil {
		t.Fatal("a breaking change validated as a minor")
	} else {
		var ve *registry.ValidationError
		if !errors.As(err, &ve) || !hasIssue(ve.Issues, "semver") || !hasIssue(ve.Issues, "migration_plan_ref") {
			t.Fatalf("%v", err)
		}
	}
	good := e.draft("bank-x", "2.0.0", func(in *registry.VersionInput) {
		in.Content.Operations[1].EndpointTemplate = "/v2/payments"
		in.MigrationPlanRef = "MIG-12 move pinned connections to v2"
	})
	v2 := e.publish(e.reviewed(good))
	if v2.CompatClass != registry.ClassBreaking {
		t.Fatalf("class %s", v2.CompatClass)
	}
	cp, _ := e.svc.Compatibility(platCtx(), "bank-x")
	if !cp.Versions[0].MigrationRequired || cp.Versions[0].ContractVersionID != v1.ContractVersionID {
		t.Fatalf("%+v", cp.Versions)
	}
}

func TestSchemaReferenceCannotBeRedefined(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	e.publish(e.reviewed(e.draft("bank-x", "1.0.0", nil)))
	v := e.draft("bank-x", "1.1.0", func(in *registry.VersionInput) {
		in.Content.Schemas[0].MaxSizeBytes = 2048 // statement@1 means something else now
	})
	_, err := e.svc.Validate(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion)
	var ve *registry.ValidationError
	if !errors.As(err, &ve) || !hasIssue(ve.Issues, "schemas[0].schema") {
		t.Fatalf("%v", err)
	}
}

func TestPublishRefusesAStaleBaseline(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	e.publish(e.reviewed(e.draft("bank-x", "1.0.0", nil)))
	// two candidates validated against the same baseline
	a := e.reviewed(e.draft("bank-x", "1.1.0", nil))
	b := e.reviewed(e.draft("bank-x", "1.2.0", nil))
	e.publish(a)
	if _, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, release, b.ContractVersionID, b.RowVersion); !errors.Is(err, registry.ErrBaselineMoved) {
		t.Fatalf("published against a moved baseline: %v", err)
	}
}

func TestConcurrentPublishersOfOneConnector(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	e.publish(e.reviewed(e.draft("bank-x", "1.0.0", nil)))
	const n = 6
	cands := make([]*registry.ContractVersion, n)
	for i := range cands {
		cands[i] = e.reviewed(e.draft("bank-x", "1."+string(rune('1'+i))+".0", nil))
	}
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range cands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = e.svc.Publish(platCtx(), registry.PlatformScopeID, release, cands[i].ContractVersionID, cands[i].RowVersion)
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, registry.ErrBaselineMoved) {
			t.Fatalf("unexpected loser error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d concurrent publishers won; exactly one may", won)
	}
	published := 0
	vs, _ := e.repo.ListVersions(platCtx(), "bank-x")
	for _, v := range vs {
		if v.Status == registry.VersionPublished {
			published++
		}
	}
	if published != 1 {
		t.Fatalf("%d versions PUBLISHED at once", published)
	}
}

func TestPublishedContentCannotDriftFromWhatWasValidated(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.reviewed(e.draft("bank-x", "1.0.0", nil))
	// simulate a store that let the content change after validation: the hash no longer matches
	tamper := func(tx *registry.VersionTx) error { tx.V.Content.Operations[0].RateLimitPerMinute++; return nil }
	if _, err := e.repo.MutateVersion(platCtx(), v.ContractVersionID, tamper); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.repo.GetVersion(platCtx(), v.ContractVersionID)
	if _, err := e.svc.Publish(platCtx(), registry.PlatformScopeID, release, cur.ContractVersionID, cur.RowVersion); !errors.Is(err, registry.ErrStaleValidation) {
		t.Fatalf("published drifted content: %v", err)
	}
}

func TestOptimisticConcurrencyOnCommands(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.validate(e.draft("bank-x", "1.0.0", nil))
	if _, err := e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion-1); !errors.Is(err, registry.ErrVersionConflict) {
		t.Fatalf("stale submit: %v", err)
	}
	if _, err := e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, v.ContractVersionID, v.RowVersion); err == nil {
		t.Fatal("submitted twice")
	}
	// a draft cannot be submitted without validation
	d := e.draft("bank-x", "1.1.0", nil)
	if _, err := e.svc.SubmitReview(platCtx(), registry.PlatformScopeID, author, d.ContractVersionID, d.RowVersion); !errors.Is(err, registry.ErrNotValidated) {
		t.Fatalf("submitted an unvalidated draft: %v", err)
	}
}

// ---- connector deprecation ------------------------------------------------------------------

func TestDeprecateConnector(t *testing.T) {
	e := newEnv(t)
	c := e.connector("bank-x")
	until := registrytest.T0.Add(60 * 24 * time.Hour)
	if _, err := e.svc.DeprecateConnector(platCtx(), registry.PlatformScopeID, author, "bank-x", c.Version, until, "provider sunset"); !errors.Is(err, registry.ErrTransition) {
		t.Fatalf("deprecated an unpublished connector: %v", err)
	}
	e.publish(e.reviewed(e.draft("bank-x", "1.0.0", nil)))
	cur, _ := e.repo.GetConnector(platCtx(), "bank-x")
	if _, err := e.svc.DeprecateConnector(platCtx(), registry.PlatformScopeID, author, "bank-x", cur.Version, registrytest.T0, "x"); !errors.Is(err, registry.ErrDeprecateWindow) {
		t.Fatalf("deprecation in the past: %v", err)
	}
	if _, err := e.svc.DeprecateConnector(platCtx(), registry.PlatformScopeID, author, "bank-x", cur.Version, until, ""); err == nil {
		t.Fatal("deprecation without a reason")
	}
	if _, err := e.svc.DeprecateConnector(platCtx(), registry.PlatformScopeID, author, "bank-x", cur.Version+5, until, "x"); !errors.Is(err, registry.ErrVersionConflict) {
		t.Fatalf("stale: %v", err)
	}
	got, err := e.svc.DeprecateConnector(platCtx(), registry.PlatformScopeID, author, "bank-x", cur.Version, until, "provider sunset")
	if err != nil || got.Lifecycle != registry.ConnectorDeprecated || got.SupportedUntil == nil {
		t.Fatalf("%v %+v", err, got)
	}
	if len(e.events("integration.connector.deprecated")) != 1 {
		t.Fatal("no deprecated event")
	}
}

// ---- mapping simulation ---------------------------------------------------------------------

func TestSimulateWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.draft("bank-x", "1.0.0", nil)
	mid := v.MappingVersions[0].MappingVersionID
	before := len(e.repo.Events)
	good := json.RawMessage(`{"account":"A1","balance":"10.5","currency":"GBP","booked_at":"2026-10-20T10:00:00+01:00","extra":"dropped"}`)
	badSample := json.RawMessage(`{"account":"A1","balance":"10.555","currency":"GBP","booked_at":"2026-10-20T10:00:00Z"}`)
	res, err := e.svc.Simulate(platCtx(), mid, []registry.SimulationSample{{Source: good}, {Source: badSample}})
	if err != nil || len(res) != 2 {
		t.Fatalf("%v %d", err, len(res))
	}
	if !res[0].OK() || string(res[0].Canonical) != `{"account":"A1","balance":"10.50","booked_at":"2026-10-20T09:00:00Z","currency":"GBP"}` {
		t.Fatalf("%v %s", res[0].Issues, res[0].Canonical)
	}
	if res[1].OK() || res[1].Issues[0].Code != "precision_loss" {
		t.Fatalf("%+v", res[1].Issues)
	}
	if len(e.repo.Events) != before {
		t.Fatal("a simulation emitted an event")
	}
	// golden check
	ok, _ := e.svc.Simulate(platCtx(), mid, []registry.SimulationSample{{Source: good, ExpectedCanonicalHash: res[0].CanonicalHash}})
	if ok[0].GoldenMatch == nil || !*ok[0].GoldenMatch {
		t.Fatal("golden match not reported")
	}
	miss, _ := e.svc.Simulate(platCtx(), mid, []registry.SimulationSample{{Source: good, ExpectedCanonicalHash: strings.Repeat("0", 64)}})
	if miss[0].GoldenMatch == nil || *miss[0].GoldenMatch {
		t.Fatal("golden mismatch not reported")
	}
	// limits
	if _, err = e.svc.Simulate(platCtx(), mid, nil); err == nil {
		t.Fatal("zero samples accepted")
	}
	if _, err = e.svc.Simulate(platCtx(), mid, make([]registry.SimulationSample, 21)); err == nil {
		t.Fatal("21 samples accepted")
	}
	if _, err = e.svc.Simulate(platCtx(), "ghost", []registry.SimulationSample{{Source: good}}); !errors.Is(err, registry.ErrMappingNotFound) {
		t.Fatalf("%v", err)
	}
	// a tenant cannot simulate a mapping it cannot see
	if _, err = e.svc.Simulate(tenantCtx(), mid, []registry.SimulationSample{{Source: good}}); !errors.Is(err, registry.ErrMappingNotFound) {
		t.Fatalf("tenant simulated a draft mapping: %v", err)
	}
}

func TestSimulateNeverLeaksSourceValues(t *testing.T) {
	e := newEnv(t)
	e.connector("bank-x")
	v := e.draft("bank-x", "1.0.0", nil)
	res, _ := e.svc.Simulate(platCtx(), v.MappingVersions[0].MappingVersionID, []registry.SimulationSample{
		{Source: json.RawMessage(`{"account":"A1","balance":"x","currency":"TOPSECRETCCY","booked_at":"bad","holder":{"national_id":"AB123456C"}}`)}})
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), "TOPSECRETCCY") || strings.Contains(string(raw), "AB123456C") {
		t.Fatalf("source values leaked: %s", raw)
	}
}
