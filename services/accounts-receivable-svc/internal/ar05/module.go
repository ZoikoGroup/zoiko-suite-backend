package ar05

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/kit"
)

// Collaborators are the injected ports of AR-05. Pass a literal nil (never a typed-nil pointer) for one that is not wired.
type Collaborators struct {
	// Invoices is AR-04's kit.Invoices (the source of issued invoices and the e-invoice dimension writer). Required.
	Invoices kit.Invoices
	// Profiles resolves the e-invoice profile from the jurisdiction pack. nil: Prepare answers 503 and Required errors.
	Profiles ProfileResolver
	// Catalog says which profile code a jurisdiction uses (deployment configuration). nil: the command must carry profile_code
	// and Required answers false.
	Catalog ProfileCatalog
	// Entities gives the jurisdiction of a legal entity, which the profile must match. nil: Prepare answers 503 (never a guess).
	Entities EntityJurisdictions
	// Registry registers submissions in jurisdiction-rules-svc's regulatory submission registry. nil: local-only records.
	Registry Registry
	// Transports selects the wire per profile. nil: UnconfiguredTransports (submission answers 503 EINVOICE_PROVIDER_NOT_CONFIGURED).
	Transports TransportResolver
	// Secrets supplies the per-provider callback secrets. nil: EnvSecrets (AR_EINVOICE_CALLBACK_SECRET_<PROVIDER>).
	Secrets SecretSource
	// Sources are extra document sources by document type (AR-06 registers CREDIT_NOTE / DEBIT_NOTE here). INVOICE is built in.
	Sources map[string]DocumentSource
	// Renderers are extra renderers by syntax; the canonical JSON renderer is built in.
	Renderers []Renderer
}

// Module is AR-05. It implements kit.EInvoiceRequirement (consumed by AR-04 at issue).
type Module struct {
	deps       kit.Deps
	profiles   ProfileResolver
	catalog    ProfileCatalog
	entities   EntityJurisdictions
	registry   Registry
	transports TransportResolver
	secrets    SecretSource
	sources    map[string]DocumentSource
	renderers  Renderers

	// SubmitTimeout bounds one Submit / QueryStatus call. When it fires the outcome is AMBIGUOUS, never "failed".
	SubmitTimeout time.Duration
	// CallbackWindow is the accepted age/skew of a signed callback.
	CallbackWindow time.Duration
}

var _ kit.EInvoiceRequirement = (*Module)(nil)

// New builds the module.
func New(deps kit.Deps, c Collaborators) *Module {
	m := &Module{deps: deps, profiles: c.Profiles, catalog: c.Catalog, entities: c.Entities, registry: c.Registry, transports: c.Transports, secrets: c.Secrets,
		sources: map[string]DocumentSource{}, renderers: DefaultRenderers(), SubmitTimeout: 30 * time.Second, CallbackWindow: DefaultCallbackWindow}
	if m.transports == nil {
		m.transports = UnconfiguredTransports{}
	}
	if m.secrets == nil {
		m.secrets = EnvSecrets{}
	}
	if c.Invoices != nil {
		m.sources[DocInvoice] = InvoiceSource{Invoices: c.Invoices}
	}
	for k, v := range c.Sources {
		m.sources[k] = v
	}
	for _, r := range c.Renderers {
		m.renderers[r.Syntax()] = r
	}
	return m
}

// RegisterCallbackPath is the one route that is NOT behind the user Guard: the provider has no principal. It is
// authenticated solely by its HMAC signature (callback.go).
const CallbackPath = "/internal/v1/ar05/commands/recordProviderCallback"

// Register mounts the spec paths verbatim at the router root.
func (m *Module) Register(r chi.Router) {
	for _, q := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"getEInvoiceStatus", m.getEInvoiceStatus},
		{"getSubmissionAttempt", m.getSubmissionAttempt},
		{"getValidationErrors", m.getValidationErrors},
		{"getAcknowledgement", m.getAcknowledgement},
		{"listSubmissionHistory", m.listSubmissionHistory},
	} {
		h := m.deps.Query(q.h)
		r.Get("/internal/v1/ar05/"+q.name, h)
		r.Post("/internal/v1/ar05/"+q.name, h)
	}
	for _, c := range []struct {
		path, op string
		h        http.HandlerFunc
	}{
		{"prepareEInvoice", "AR05.PrepareEInvoice", m.prepareEInvoice},
		{"validateEInvoice", "AR05.ValidateEInvoice", m.validateEInvoice},
		{"submitEInvoice", "AR05.SubmitEInvoice", m.submitEInvoice},
		{"retrySubmission", "AR05.RetrySubmission", m.retrySubmission},
		{"reconcileSubmissionState", "AR05.ReconcileSubmissionState", m.reconcileSubmissionState},
		{"cancelSubmissionWherePermitted", "AR05.CancelSubmissionWherePermitted", m.cancelSubmission},
	} {
		r.Post("/internal/v1/ar05/commands/"+c.path, m.deps.Cmd(c.op, c.h))
	}
	// Provider callback: no Idempotency-Key, no principal. Replay safety is the (attempt, provider_event_id) unique key
	// plus the single-use nonce.
	r.Post(CallbackPath, m.recordProviderCallback)
}

func (m *Module) read(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return m.deps.DB.WithTx(ctx, tenantID, fn)
}

func (m *Module) now() time.Time { return m.deps.Now() }

func (m *Module) fail(w http.ResponseWriter, err error) {
	var p *kit.Problem
	if !errors.As(err, &p) {
		m.deps.Log.Error("ar05 request failed", zap.Error(err))
	}
	kit.WriteErr(w, err)
}

// checkAction is Authorize without writing the response.
func (m *Module) checkAction(ctx context.Context, c kit.Caller, action string) error {
	err := m.deps.Authz.CheckAllowed(ctx, c.PrincipalID, c.LegalEntityID, action)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrAuthorizationDenied):
		return kit.Forbidden(kit.CodeForbidden, "")
	default:
		m.deps.Log.Error("authorization check failed - failing closed", zap.Error(err))
		return kit.Unavailable("authorization service unavailable")
	}
}

// refusal keeps audit evidence (<command>.denied) of a refused attempt: the command wrote nothing else.
func (m *Module) refusal(ctx context.Context, c kit.Caller, aggregateID, command string, p *kit.Problem, detail map[string]any) {
	if m.deps.DB == nil {
		return
	}
	d := map[string]any{"denial": p.Code, "message": p.Msg}
	for k, v := range detail {
		d[k] = v
	}
	if err := m.deps.DB.WithTx(ctx, c.TenantID, func(tx pgx.Tx) error {
		return kit.RecordAudit(ctx, tx, c, kit.Audit{AggregateType: AggregateType, AggregateID: aggregateID, Command: command + ".denied", Detail: d})
	}); err != nil {
		m.deps.Log.Error("ar05 could not record a refusal", zap.Error(err))
	}
}

func (m *Module) source(docType string) (DocumentSource, error) {
	if s, ok := m.sources[docType]; ok && s != nil {
		return s, nil
	}
	return nil, validationFailed("e-invoicing of %s documents is not available (no document source is wired)", docType)
}

func evPayload(c kit.Caller, s *Submission, extra map[string]any) map[string]any {
	p := map[string]any{"submission_id": s.SubmissionID, "legal_entity_id": s.LegalEntityID, "document_type": s.DocumentType, "document_id": s.DocumentID,
		"document_number": s.DocumentNumber, "profile_code": s.ProfileCode, "profile_version": s.ProfileVersion, "network": s.Network, "state": s.State,
		"version": s.Version, "content_hash": s.ContentHash, "artifact_hash": s.ArtifactHash, "correlation_id": c.CorrelationID}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// ── kit.EInvoiceRequirement ──────────────────────────────────────────────────

// Required reports whether an e-invoice must follow an issued document. It is answered from the resolved profile: the
// jurisdiction has a profile in force for the document type -> true. No profile (nothing configured for the jurisdiction,
// no rule in force, unsupported jurisdiction) -> false. A resolver that cannot answer is an ERROR, so AR-04 fails closed
// instead of issuing a document whose e-invoice obligation is unknown.
func (m *Module) Required(ctx context.Context, tenantID, legalEntityID, jurisdictionID, documentType string) (bool, error) {
	if m.catalog == nil {
		return false, nil
	}
	code, ok := m.catalog.CodeFor(jurisdictionID, documentType)
	if !ok {
		return false, nil
	}
	if m.profiles == nil {
		return false, errors.New("ar05: e-invoice profile resolver not wired")
	}
	p, err := m.profiles.Resolve(ctx, kit.Caller{TenantID: tenantID, PrincipalID: "system:ar05-einvoice", LegalEntityID: legalEntityID},
		ProfileQuery{Jurisdiction: jurisdictionID, ProfileCode: code, DocumentType: documentType, At: m.now()})
	switch {
	case err == nil:
		return p != nil && p.DocumentType == documentType, nil
	case errors.Is(err, ErrNoProfile):
		return false, nil
	default:
		return false, err
	}
}

// ── small helpers shared by handlers ─────────────────────────────────────────

func available(s State) []string {
	switch s {
	case StatePrepared:
		return []string{"validateEInvoice", "cancelSubmissionWherePermitted"}
	case StateValidated:
		return []string{"submitEInvoice", "cancelSubmissionWherePermitted"}
	case StateSubmitted:
		return []string{"reconcileSubmissionState"}
	case StateUnknown:
		return []string{"retrySubmission", "reconcileSubmissionState"}
	case StateRejected:
		return []string{"prepareEInvoice"} // a chained resubmission
	default:
		return []string{}
	}
}

func upperTrim(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
