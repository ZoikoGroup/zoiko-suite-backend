package ar05

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

// systemCaller is the identity used for service-to-service reads that have no user (Required, entity lookups).
func systemCaller(tenantID string) svcclient.Caller {
	return svcclient.Caller{TenantID: tenantID, PrincipalID: "system:ar05-einvoice"}
}

// ── document source ──────────────────────────────────────────────────────────

// DocumentSource reads one kind of issued document and mirrors the submission state onto its e-invoice dimension.
// Only INVOICE is wired (InvoiceSource over kit.Invoices); AR-06 plugs a credit/debit note source in through
// Collaborators.Sources. Get returns NOT_FOUND for anything not issued, so a draft can never be transmitted.
type DocumentSource interface {
	Get(ctx context.Context, tenantID, documentID string) (*SourceDocument, error)
	SetEInvoiceState(ctx context.Context, tx pgx.Tx, c kit.Caller, documentID, state string) error
}

// InvoiceSource is the DocumentSource of invoices, backed by AR-04's kit.Invoices.
type InvoiceSource struct{ Invoices kit.Invoices }

func (s InvoiceSource) Get(ctx context.Context, tenantID, documentID string) (*SourceDocument, error) {
	in, err := s.Invoices.GetIssued(ctx, tenantID, documentID)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, kit.NotFound("document not found")
	}
	return SourceFromIssuedInvoice(in), nil
}

func (s InvoiceSource) SetEInvoiceState(ctx context.Context, tx pgx.Tx, c kit.Caller, documentID, state string) error {
	return s.Invoices.SetEInvoiceState(ctx, tx, c, documentID, state)
}

// ── profile resolution ───────────────────────────────────────────────────────

var (
	// ErrNoProfile: no e-invoice profile is in force for the jurisdiction / code / date (never guessed).
	ErrNoProfile = errors.New("no e-invoice profile in force")
	// ErrProfileInvalid: the registry answered but the profile cannot be used (no parameters, dependency unknown/expired).
	ErrProfileInvalid = errors.New("e-invoice profile unusable")
	// ErrProfileUnavailable: the registry could not give an answer; callers fail closed (503).
	ErrProfileUnavailable = errors.New("jurisdiction-rules-svc unavailable")
)

// ProfileQuery asks which e-invoice profile applies.
type ProfileQuery struct {
	Jurisdiction string
	ProfileCode  string
	DocumentType string // checked by the caller against Profile.DocumentType
	At           time.Time
}

// ProfileResolver resolves a profile from the jurisdiction pack.
type ProfileResolver interface {
	Resolve(ctx context.Context, c kit.Caller, q ProfileQuery) (*Profile, error)
}

// HTTPProfileResolver talks to jurisdiction-rules-svc POST /v1/submission-profiles:resolve.
type HTTPProfileResolver struct{ c *svcclient.Client }

func NewHTTPProfileResolver(baseURL string) *HTTPProfileResolver {
	return &HTTPProfileResolver{c: svcclient.New(baseURL)}
}

type wireProfileResponse struct {
	Profile struct {
		Outcome    string `json:"outcome"`
		Resolution *struct {
			Pack *struct {
				PackRef        string `json:"pack_ref"`
				Version        string `json:"version"`
				PackVersionID  string `json:"pack_version_id"`
				ArtifactDigest string `json:"artifact_digest"`
			} `json:"pack"`
			Rule *struct {
				RuleID           string `json:"rule_id"`
				JurisdictionCode string `json:"jurisdiction_code"`
				ContentDigest    string `json:"content_digest"`
			} `json:"rule"`
		} `json:"resolution"`
		Profile *struct {
			Channel          string         `json:"channel"`
			DocumentType     string         `json:"document_type"`
			ProfileVersion   string         `json:"profile_version"`
			Syntax           string         `json:"syntax"`
			TransmissionMode string         `json:"transmission_mode"`
			Corrections      []string       `json:"corrections"`
			Dependencies     []Dependency   `json:"dependencies"`
			SemanticMapping  []MappingEntry `json:"semantic_mapping"`
			Lifecycle        *struct {
				Initial     string     `json:"initial"`
				Terminal    []string   `json:"terminal"`
				Transitions [][]string `json:"transitions"`
			} `json:"lifecycle"`
		} `json:"profile"`
	} `json:"profile"`
	DecisionID string `json:"decision_id"`
}

func (r *HTTPProfileResolver) Resolve(ctx context.Context, c kit.Caller, q ProfileQuery) (*Profile, error) {
	body := map[string]any{"jurisdiction": q.Jurisdiction, "kind": "EINVOICE", "profile_code": q.ProfileCode,
		"effective_at": q.At.UTC().Format(time.RFC3339Nano), "dependencies_used": []any{}}
	sc := c.SvcCaller()
	sc.IdempotencyKey = "" // a resolution is a read; every answer is recorded as evidence by the registry
	status, raw, err := r.c.Do(ctx, sc, http.MethodPost, "/v1/submission-profiles:resolve", body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProfileUnavailable, err)
	}
	var w wireProfileResponse
	if status == http.StatusOK || status == http.StatusUnprocessableEntity {
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, fmt.Errorf("%w: undecodable answer", ErrProfileUnavailable)
		}
		switch w.Profile.Outcome {
		case "PROFILE_RESOLVED":
		case "NO_RULE", "UNSUPPORTED_JURISDICTION":
			return nil, fmt.Errorf("%w: %s for %s/%s", ErrNoProfile, w.Profile.Outcome, q.Jurisdiction, q.ProfileCode)
		default:
			return nil, fmt.Errorf("%w: %s", ErrProfileInvalid, w.Profile.Outcome)
		}
	} else {
		// 409 AMBIGUOUS, 503 pack_unverified, authz/validation errors: no usable answer, fail closed.
		return nil, fmt.Errorf("%w: status %d", ErrProfileUnavailable, status)
	}
	p := w.Profile.Profile
	if p == nil {
		return nil, fmt.Errorf("%w: no profile in the answer", ErrProfileInvalid)
	}
	out := &Profile{Jurisdiction: q.Jurisdiction, ProfileCode: q.ProfileCode, Network: p.Channel, DocumentType: p.DocumentType, ProfileVersion: p.ProfileVersion,
		Syntax: p.Syntax, Mode: p.TransmissionMode, Corrections: p.Corrections, Dependencies: p.Dependencies, SemanticMapping: p.SemanticMapping,
		EffectiveAt: q.At.UTC(), DecisionID: w.DecisionID}
	if p.Lifecycle != nil {
		set := map[string]bool{p.Lifecycle.Initial: true}
		for _, t := range p.Lifecycle.Transitions {
			for _, s := range t {
				set[s] = true
			}
		}
		for _, s := range p.Lifecycle.Terminal {
			set[s] = true
		}
		for s := range set {
			out.LifecycleStates = append(out.LifecycleStates, s)
		}
	}
	if res := w.Profile.Resolution; res != nil {
		if res.Pack != nil {
			out.PackRef, out.PackVersion, out.PackVersionID, out.PackDigest = res.Pack.PackRef, res.Pack.Version, res.Pack.PackVersionID, res.Pack.ArtifactDigest
		}
		if res.Rule != nil {
			out.RuleID, out.RuleDigest, out.RuleJurisdiction = res.Rule.RuleID, res.Rule.ContentDigest, res.Rule.JurisdictionCode
		}
	}
	return out, nil
}

// ProfileCatalog says which profile code (the jurisdiction pack's rule code) a jurisdiction uses for a document type.
// The registry has no "list profiles" API, so this mapping is deployment configuration, not code: no jurisdiction logic
// lives in this package.
type ProfileCatalog interface {
	CodeFor(jurisdiction, documentType string) (string, bool)
}

// StaticProfileCatalog maps "JURISDICTION|DOCTYPE" to a profile code.
type StaticProfileCatalog map[string]string

func (s StaticProfileCatalog) CodeFor(jurisdiction, documentType string) (string, bool) {
	code, ok := s[strings.ToUpper(strings.TrimSpace(jurisdiction))+"|"+strings.ToUpper(strings.TrimSpace(documentType))]
	return code, ok && code != ""
}

// ParseProfileCatalog parses "GB|INVOICE=PEPPOL_INVOICE,DE|INVOICE=XRECHNUNG_INVOICE" (the AR_EINVOICE_PROFILES value).
func ParseProfileCatalog(spec string) (StaticProfileCatalog, error) {
	out := StaticProfileCatalog{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		key := strings.Split(strings.TrimSpace(kv[0]), "|")
		if len(kv) != 2 || len(key) != 2 || strings.TrimSpace(key[0]) == "" || strings.TrimSpace(key[1]) == "" || strings.TrimSpace(kv[1]) == "" {
			return nil, fmt.Errorf("profile catalog entry %q must be JURISDICTION|DOCTYPE=PROFILE_CODE", part)
		}
		out[strings.ToUpper(strings.TrimSpace(key[0]))+"|"+strings.ToUpper(strings.TrimSpace(key[1]))] = strings.TrimSpace(kv[1])
	}
	return out, nil
}

// ── entity jurisdiction ──────────────────────────────────────────────────────

// EntityJurisdictions returns the jurisdiction code a legal entity operates in. The e-invoice profile must match it
// (spec negative path: wrong jurisdiction profile selected).
type EntityJurisdictions interface {
	PrimaryJurisdiction(ctx context.Context, tenantID, legalEntityID string) (string, error)
}

// HTTPEntityJurisdictions resolves the entity's primary_jurisdiction_id (a UUID) in tenant-entity-registry-svc and then
// its jurisdiction_code in jurisdiction-rules-svc (GET /v1/jurisdictions/{id}).
type HTTPEntityJurisdictions struct{ entities, rules *svcclient.Client }

func NewHTTPEntityJurisdictions(entityRegistryURL, jurisdictionRulesURL string) *HTTPEntityJurisdictions {
	return &HTTPEntityJurisdictions{entities: svcclient.New(entityRegistryURL), rules: svcclient.New(jurisdictionRulesURL)}
}

func (h *HTTPEntityJurisdictions) PrimaryJurisdiction(ctx context.Context, tenantID, legalEntityID string) (string, error) {
	sc := systemCaller(tenantID)
	status, raw, err := h.entities.Do(ctx, sc, http.MethodGet, "/v1/entities/"+url.PathEscape(legalEntityID), nil)
	if err != nil || status != http.StatusOK {
		return "", fmt.Errorf("%w: entity registry status %d", svcclient.ErrUnavailable, status)
	}
	var e struct {
		TenantID              string `json:"tenant_id"`
		PrimaryJurisdictionID string `json:"primary_jurisdiction_id"`
	}
	if json.Unmarshal(raw, &e) != nil || e.TenantID != tenantID || e.PrimaryJurisdictionID == "" {
		return "", fmt.Errorf("%w: entity registry gave no jurisdiction for this tenant", svcclient.ErrUnavailable)
	}
	status, raw, err = h.rules.Do(ctx, sc, http.MethodGet, "/v1/jurisdictions/"+url.PathEscape(e.PrimaryJurisdictionID), nil)
	if err != nil || status != http.StatusOK {
		return "", fmt.Errorf("%w: jurisdiction lookup status %d", svcclient.ErrUnavailable, status)
	}
	var j struct {
		Code string `json:"jurisdiction_code"`
	}
	if json.Unmarshal(raw, &j) != nil || j.Code == "" {
		return "", fmt.Errorf("%w: jurisdiction has no code", svcclient.ErrUnavailable)
	}
	return j.Code, nil
}

// ── regulatory submission registry (jurisdiction-rules-svc) ───────────────────

// RegistryRegistration is what the registry needs to register a submission under the profile in force.
type RegistryRegistration struct {
	Jurisdiction string
	ProfileCode  string
	EffectiveAt  time.Time
	SubjectRef   string // "<document type>:<document id>"
	PayloadHash  string // "sha256:<hex>" of the artifact
	Key          string // idempotency key, stable per submission
}

// RegistryEvent is a status report mirrored to the registry.
type RegistryEvent struct {
	ProviderEventID string
	Status          string // a state of the profile's registry lifecycle, e.g. ACCEPTED
	ReceiptID       string
	OccurredAt      time.Time
	Detail          map[string]any
}

// Registry registers submissions in jurisdiction-rules-svc's regulatory submission registry (optional; nil = local-only).
// The registry stores the profile snapshot and the provider status reports against the pack's lifecycle; it does not
// transmit anything.
type Registry interface {
	Register(ctx context.Context, c kit.Caller, r RegistryRegistration) (registryID string, err error)
	RecordEvent(ctx context.Context, c kit.Caller, registryID string, e RegistryEvent) error
}

type HTTPRegistry struct{ c *svcclient.Client }

func NewHTTPRegistry(baseURL string) *HTTPRegistry { return &HTTPRegistry{c: svcclient.New(baseURL)} }

func (h *HTTPRegistry) Register(ctx context.Context, c kit.Caller, r RegistryRegistration) (string, error) {
	sc := c.SvcCaller()
	sc.IdempotencyKey = r.Key
	status, raw, err := h.c.Do(ctx, sc, http.MethodPost, "/v1/regulatory-submissions", map[string]any{
		"jurisdiction": r.Jurisdiction, "kind": "EINVOICE", "profile_code": r.ProfileCode, "effective_at": r.EffectiveAt.UTC().Format(time.RFC3339Nano),
		"dependencies_used": []any{}, "subject_ref": r.SubjectRef, "payload_hash": r.PayloadHash})
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", fmt.Errorf("registry refused the registration: status %d", status)
	}
	var w struct {
		Submission struct {
			ID string `json:"submission_id"`
		} `json:"submission"`
	}
	if json.Unmarshal(raw, &w) != nil || w.Submission.ID == "" {
		return "", fmt.Errorf("registry answered without a submission id")
	}
	return w.Submission.ID, nil
}

func (h *HTTPRegistry) RecordEvent(ctx context.Context, c kit.Caller, registryID string, e RegistryEvent) error {
	sc := c.SvcCaller()
	sc.IdempotencyKey = ""
	body := map[string]any{"provider_event_id": e.ProviderEventID, "status": e.Status, "receipt_id": e.ReceiptID,
		"occurred_at": e.OccurredAt.UTC().Format(time.RFC3339Nano)}
	if len(e.Detail) > 0 {
		body["detail"] = e.Detail
	}
	status, _, err := h.c.Do(ctx, sc, http.MethodPost, "/v1/regulatory-submissions/"+url.PathEscape(registryID)+"/events", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("registry refused the event: status %d", status)
	}
	return nil
}
