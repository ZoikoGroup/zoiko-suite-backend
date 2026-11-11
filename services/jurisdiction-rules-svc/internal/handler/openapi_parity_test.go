package handler_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
	"zoiko.io/jurisdiction-rules-svc/internal/resolver"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// stubRegistryStore implements handler.RegistryStore for route parity testing.
type stubRegistryStore struct {
	stubStore
}

func (s *stubRegistryStore) CreateRegime(ctx context.Context, p domain.CreateRegimeParams) (*domain.Regime, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetRegime(ctx context.Context, idOrCode string) (*domain.Regime, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListRegimes(ctx context.Context) ([]*domain.Regime, error) {
	return nil, nil
}
func (s *stubRegistryStore) CreateSource(ctx context.Context, p domain.CreateSourceParams) (*domain.RegulatorySource, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetSource(ctx context.Context, id string) (*domain.RegulatorySource, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListSources(ctx context.Context, jurisdictionID, authority string, limit, offset int) ([]*domain.RegulatorySource, error) {
	return nil, nil
}
func (s *stubRegistryStore) ReviewSource(ctx context.Context, id, reviewer string) (*domain.RegulatorySource, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) SupersedeSource(ctx context.Context, id, replacementID string) (*domain.RegulatorySource, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) CreateInterpretation(ctx context.Context, p domain.CreateInterpretationParams) (*domain.InterpretationRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) GetInterpretation(ctx context.Context, id string) (*domain.InterpretationRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) ApproveInterpretation(ctx context.Context, id, approver string) (*domain.InterpretationRecord, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) CreatePack(ctx context.Context, p domain.CreatePackParams) (*domain.Pack, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetPack(ctx context.Context, ref string) (*domain.Pack, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListPacks(ctx context.Context, limit, offset int) ([]*domain.Pack, error) {
	return nil, nil
}
func (s *stubRegistryStore) CreatePackVersion(ctx context.Context, p domain.CreatePackVersionParams) (*domain.PackVersion, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetPackVersion(ctx context.Context, ref, version string) (*domain.PackVersion, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListPackVersions(ctx context.Context, ref string) ([]*domain.PackVersion, error) {
	return nil, nil
}
func (s *stubRegistryStore) SubmitPackVersionForReview(ctx context.Context, ref, version, actor string) (*domain.PackVersion, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) SetRuleProvenance(ctx context.Context, p domain.SetRuleProvenanceParams) (*domain.RuleProvenance, error) {
	return nil, nil
}
func (s *stubRegistryStore) GetRuleProvenance(ctx context.Context, ruleID string) (*domain.RuleProvenance, error) {
	return nil, nil
}
func (s *stubRegistryStore) RegisterSigningKey(ctx context.Context, keyRef, algorithm string, pub []byte, actor string) (*domain.TrustedKey, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetSigningKey(ctx context.Context, keyRef string) (*domain.TrustedKey, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListSigningKeys(ctx context.Context) ([]*domain.TrustedKey, error) {
	return nil, nil
}
func (s *stubRegistryStore) SetSigningKeyStatus(ctx context.Context, keyRef, status, reason, actor string) (*domain.TrustedKey, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) CompilePackVersion(ctx context.Context, ref, version, actor string) (*store.PackArtifact, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetPackArtifact(ctx context.Context, ref, version string) (*store.PackArtifact, error) {
	return nil, nil
}
func (s *stubRegistryStore) SignPackVersion(ctx context.Context, ref, version string, signer domain.Signer, actor string) (*store.PackArtifact, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) VerifyPackArtifact(ctx context.Context, ref, version string) (*store.PackArtifact, domain.VerificationResult, error) {
	return nil, domain.VerificationResult{}, nil
}
func (s *stubRegistryStore) SubmitTestBundle(ctx context.Context, ref, version string, canonical []byte, digest, actor string) (*store.TestBundleRecord, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetLatestTestBundle(ctx context.Context, ref, version string) (*store.TestBundleRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) RunTests(ctx context.Context, ref, version, actor string) (*store.TestRunRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListTestRuns(ctx context.Context, ref, version string, limit int) ([]*store.TestRunRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) AddReview(ctx context.Context, ref, version, reviewer, role, decision, findings string) (*store.ReviewRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListReviews(ctx context.Context, ref, version string) ([]*store.ReviewRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) CertifyPackVersion(ctx context.Context, ref, version, certifier string, minReviews int, signer domain.Signer) (*store.CertificationRecord, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetCertification(ctx context.Context, ref, version string) (*store.CertificationRecord, domain.VerificationResult, error) {
	return nil, domain.VerificationResult{}, nil
}
func (s *stubRegistryStore) RecordDecision(ctx context.Context, rec store.DecisionRecord) (*store.StoredDecision, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetDecision(ctx context.Context, id string) (*store.StoredDecision, error) {
	return nil, nil
}
func (s *stubRegistryStore) CreateRollout(ctx context.Context, p store.CreateRolloutParams) (*store.RolloutRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) SeedPortfolio(ctx context.Context, actor string) (int, int, error) {
	return 0, 0, nil
}
func (s *stubRegistryStore) ListRollouts(ctx context.Context, status string, limit, offset int) ([]*store.RolloutRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) GetRollout(ctx context.Context, id string) (*store.RolloutDetail, error) {
	return nil, nil
}
func (s *stubRegistryStore) SetRolloutOwner(ctx context.Context, id, owner string, supportOwner *string) (*store.RolloutRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) LinkRolloutPacks(ctx context.Context, id string, packRefs []string, actor string) error {
	return nil
}
func (s *stubRegistryStore) AddAttestation(ctx context.Context, id, checklist, item string, met bool, evidence, actor string) error {
	return nil
}
func (s *stubRegistryStore) AddExpertApproval(ctx context.Context, id, expert, qualification, scope, decision, notes string) error {
	return nil
}
func (s *stubRegistryStore) TransitionRollout(ctx context.Context, id, to, actor, reason string) (*store.RolloutRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) JurisdictionSupport(ctx context.Context, code string) (*store.SupportView, error) {
	return nil, nil
}
func (s *stubRegistryStore) CreateSubmission(ctx context.Context, p store.CreateSubmissionParams) (*store.SubmissionRecord, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetSubmission(ctx context.Context, id string) (*store.SubmissionRecord, []*store.SubmissionEventRecord, error) {
	return nil, nil, nil
}
func (s *stubRegistryStore) RecordSubmissionEvent(ctx context.Context, id string, p store.SubmissionEventParams) (*store.SubmissionEventResult, error) {
	return nil, nil
}
func (s *stubRegistryStore) SetRuleParameters(ctx context.Context, ruleID string, params json.RawMessage, actor string) (json.RawMessage, error) {
	return nil, nil
}
func (s *stubRegistryStore) GetRuleParameters(ctx context.Context, ruleID string) (json.RawMessage, error) {
	return nil, nil
}
func (s *stubRegistryStore) CreateCalendar(ctx context.Context, code, jurisdictionID, authority string, description *string, actor string) (*store.Calendar, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetCalendar(ctx context.Context, code string) (*store.Calendar, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListCalendars(ctx context.Context) ([]*store.Calendar, error) {
	return nil, nil
}
func (s *stubRegistryStore) CreateCalendarVersion(ctx context.Context, code, effectiveFrom, tz string, weekend []int, cutoff *string, holidays []domain.Holiday, actor string) (*store.CalendarVersionRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) GetCalendarVersion(ctx context.Context, code string, version int) (*store.CalendarVersionRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListCalendarVersions(ctx context.Context, code string) ([]*store.CalendarVersionRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) SetCalendarVersionSources(ctx context.Context, code string, version int, sourceIDs []string) (*store.CalendarVersionRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) PublishCalendarVersion(ctx context.Context, code string, version int, actor string) (*store.CalendarVersionRecord, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) CreateObligationRule(ctx context.Context, p store.CreateObligationParams) (*store.ObligationRuleRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) GetObligationRule(ctx context.Context, id string) (*store.ObligationRuleRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListObligationRules(ctx context.Context, jurisdictionID, code string, limit, offset int) ([]*store.ObligationRuleRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) SetObligationProvenance(ctx context.Context, id string, regimeID, interpretationID *string, sourceIDs []string) (*store.ObligationRuleRecord, error) {
	return nil, nil
}
func (s *stubRegistryStore) PublishObligationRule(ctx context.Context, id, actor string) (*store.ObligationRuleRecord, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) CreateRegion(ctx context.Context, code string, description *string, actor string) (*store.Region, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) ListRegions(ctx context.Context) ([]*store.Region, error) {
	return nil, nil
}
func (s *stubRegistryStore) CreateRing(ctx context.Context, code string, ordinal, soak int, description *string, actor string) (*store.Ring, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) ListRings(ctx context.Context) ([]*store.Ring, error) {
	return nil, nil
}
func (s *stubRegistryStore) PublishPackVersion(ctx context.Context, ref, version, actor, evidenceRef string) (*store.ReleaseResult, error) {
	return nil, nil
}
func (s *stubRegistryStore) SupersedePackVersion(ctx context.Context, ref, version, byVersion, reason, evidenceRef, actor string) (*store.ReleaseResult, error) {
	return nil, nil
}
func (s *stubRegistryStore) OpenRemediationCase(ctx context.Context, ref, version, ring, region, reason, evidence, actor string) (*store.RemediationCase, error) {
	return nil, nil
}
func (s *stubRegistryStore) GetRemediationCase(ctx context.Context, id string) (*store.RemediationCase, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListRemediationCases(ctx context.Context, status string, limit, offset int) ([]*store.RemediationCase, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListRemediationDecisions(ctx context.Context, caseID string, pendingOnly bool, limit, offset int) ([]*store.RemediationDecision, error) {
	return nil, nil
}
func (s *stubRegistryStore) RecordDispositions(ctx context.Context, caseID string, in []domain.DispositionInput, actor string) (*store.RecordDispositionsResult, error) {
	return nil, nil
}
func (s *stubRegistryStore) CloseRemediationCase(ctx context.Context, id, note, actor string) (*store.RemediationCase, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) WithdrawPackVersion(ctx context.Context, ref, version, reason, evidenceRef, actor string) (*store.ReleaseResult, error) {
	return nil, nil
}
func (s *stubRegistryStore) BlockPackVersion(ctx context.Context, ref, version, reason, actor string) (*store.ReleaseResult, error) {
	return nil, nil
}
func (s *stubRegistryStore) UnblockPackVersion(ctx context.Context, ref, version, reason, actor string) (*store.ReleaseResult, error) {
	return nil, nil
}
func (s *stubRegistryStore) DeployPackVersion(ctx context.Context, ref, version, ring, region, actor, exceptionReason string) (*store.Deployment, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) RollbackDeployment(ctx context.Context, ref, version, ring, region, restoreVersion, reason, actor string) (*store.RollbackResult, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListDeployments(ctx context.Context, ref, version string) ([]*store.Deployment, error) {
	return nil, nil
}
func (s *stubRegistryStore) DeclareHotfix(ctx context.Context, ref, version, severity, scope, incident, commander, rollbackTarget string, retroSLA time.Duration, actor string) (*store.Hotfix, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) GetHotfix(ctx context.Context, ref, version string) (*store.Hotfix, error) {
	return nil, nil
}
func (s *stubRegistryStore) CompleteRetrospective(ctx context.Context, ref, version, note, actor string) (*store.Hotfix, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) CreateNotice(ctx context.Context, p store.CreateNoticeParams) (*store.SourceChangeNotice, error) {
	return nil, nil
}
func (s *stubRegistryStore) GetNotice(ctx context.Context, id string) (*store.SourceChangeNotice, error) {
	return nil, nil
}
func (s *stubRegistryStore) ListNotices(ctx context.Context, status string, limit, offset int) ([]*store.SourceChangeNotice, error) {
	return nil, nil
}
func (s *stubRegistryStore) StartNoticeReview(ctx context.Context, id, reviewer string) (*store.SourceChangeNotice, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) CloseNotice(ctx context.Context, id, closer, outcome, note string, linkedInterpretation *string) (*store.SourceChangeNotice, bool, error) {
	return nil, false, nil
}
func (s *stubRegistryStore) Metrics(ctx context.Context, windowHours, certAgeWarnDays, upcomingDays int) (*store.OpsMetrics, error) {
	return nil, nil
}

// stubRegistryPub implements events.RegistryPublisher for route parity testing.
type stubRegistryPub struct{}

func (s *stubRegistryPub) PublishRegistryEvent(ctx context.Context, eventType, aggregateID, actorID, correlationID string, payload map[string]any) error {
	return nil
}

// stubSource implements resolver.Source for route parity testing.
type stubSource struct{}

func (s *stubSource) ListEligiblePacks(ctx context.Context, statuses []string, ring, region string) ([]domain.EligiblePack, error) {
	return nil, nil
}
func (s *stubSource) FindPackVersion(ctx context.Context, packRef, version string) (*domain.EligiblePack, error) {
	return nil, nil
}
func (s *stubSource) LoadPack(ctx context.Context, packVersionID string) (*domain.LoadedPack, error) {
	return nil, nil
}

// stubEntityReader implements handler.EntityApplicabilityReader for route parity testing.
type stubEntityReader struct{}

func (s *stubEntityReader) ListEntityJurisdictions(ctx context.Context, tenantID, legalEntityID string, at time.Time) ([]*domain.EntityJurisdictionApplicability, error) {
	return nil, nil
}

// TestOpenAPIMatchesTheRouter keeps openapi.yaml and the router in step: every
// routed operation is documented and every documented operation is routed.
// (Validity of the document itself is checked with kin-openapi; route parity
// alone once passed for a document that was not valid OpenAPI.)
func TestOpenAPIMatchesTheRouter(t *testing.T) {
	f, err := os.Open("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pathRe := regexp.MustCompile(`^  (/\S+):\s*$`)
	opRe := regexp.MustCompile(`^    (get|post|put|patch|delete):`)
	documented := map[string]bool{}
	current := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if m := pathRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if strings.HasPrefix(line, "components:") {
			current = ""
		}
		if m := opRe.FindStringSubmatch(line); m != nil && current != "" {
			if current == "/healthz" || current == "/readyz" || current == "/metrics" {
				continue // mounted in main.go
			}
			documented[strings.ToUpper(m[1])+" "+current] = true
		}
	}

	r := chi.NewRouter()
	stub := &stubRegistryStore{}
	pub := &stubRegistryPub{}
	entityReader := &stubEntityReader{}
	resolverSrc := &stubSource{}
	res, _ := resolver.New(resolverSrc, resolver.Config{
		EligibleStatuses: []string{"RELEASED"},
		CacheTTL:         0,
		Ring:             "",
		Region:           "",
		SupportGate:      nil,
	})
	h := handler.New(stub, authz.NewStubAuthZClient(zap.NewNop()), &spyPublisher{}, "", zap.NewNop())
	h.WithRegistry(stub, pub).WithCertificationPolicy(1)
	h.WithOperations(handler.OpsPolicy{})
	h.WithEntityApplicability(entityReader).WithResolver(res)
	handler.RegisterRoutes(r, h)
	routed := map[string]bool{}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routed[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})

	var missingDoc, missingRoute []string
	for op := range routed {
		if !documented[op] {
			missingDoc = append(missingDoc, op)
		}
	}
	for op := range documented {
		if !routed[op] {
			missingRoute = append(missingRoute, op)
		}
	}
	sort.Strings(missingDoc)
	sort.Strings(missingRoute)
	if len(missingDoc) > 0 || len(missingRoute) > 0 {
		t.Fatalf("openapi.yaml out of step with the router\nrouted, not documented: %v\ndocumented, not routed: %v", missingDoc, missingRoute)
	}
	if len(routed) < 20 {
		t.Fatalf("only %d routes walked; the parity check is not seeing the router", len(routed))
	}
}
