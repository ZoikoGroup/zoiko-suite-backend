package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

type Store interface {
	CreateFiscalPeriod(ctx context.Context, fp *domain.FiscalPeriod) (created bool, err error)
	GetFiscalPeriod(ctx context.Context, id string) (*domain.FiscalPeriod, error)
	GetFiscalPeriodByName(ctx context.Context, legalEntityID, name string) (*domain.FiscalPeriod, error)
	ListFiscalPeriods(ctx context.Context, legalEntityID string) ([]domain.FiscalPeriod, error)
	LockFiscalPeriod(ctx context.Context, id string, lockedAt time.Time, evidenceDocID string) error
	CreateCloseEvidence(ctx context.Context, evidence *domain.CloseEvidence) error
}

type Publisher interface {
	PublishCloseStarted(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod)
	PublishCloseBlocked(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod, reasons []string)
	PublishClosed(ctx context.Context, correlationID, actorID string, fp domain.FiscalPeriod, evidenceID string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Clients interface {
	GetUnpostedJournalsCount(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (int, error)
	CompileTrialBalance(ctx context.Context, tenantID, legalEntityID, fiscalPeriod string) (map[string]float64, error)
	// The AP/AR counts take the period bounds: an unsettled invoice only blocks
	// the period it belongs to. Without them a single outstanding invoice
	// anywhere blocked every period forever.
	GetUnsettledAPInvoicesCount(ctx context.Context, tenantID, legalEntityID string, periodStart, periodEnd time.Time) (int, error)
	GetUnsettledARInvoicesCount(ctx context.Context, tenantID, legalEntityID string, periodStart, periodEnd time.Time) (int, error)
	UploadCloseEvidence(ctx context.Context, tenantID, legalEntityID, periodName string, trialBalance map[string]float64, principalID string) (string, error)
}

const (
	actionCloseConfig   = "PERIOD_CLOSE_CONFIG"
	actionCloseView     = "PERIOD_CLOSE_VIEW"
	actionCloseInitiate = "PERIOD_CLOSE_INITIATE"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	clients   Clients
	// signingKey is the HMAC secret for close evidence. See signEvidence.
	signingKey []byte
	log        *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, clients Clients, signingKey []byte, log *zap.Logger) *Handler {
	return &Handler{
		store:      store,
		publisher:  publisher,
		authz:      authz,
		clients:    clients,
		signingKey: signingKey,
		log:        log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/close/periods", func(r chi.Router) {
		r.Post("/", h.CreateFiscalPeriod)
		r.Get("/", h.ListFiscalPeriods)
		r.Get("/status", h.GetPeriodStatus)
		r.Get("/{id}/readiness", h.GetPeriodReadiness)
		r.Post("/{id}/lock", h.LockPeriod)
	})
}

// ── POST /v1/close/periods ────────────────────────────────────────────────────────

func (h *Handler) CreateFiscalPeriod(w http.ResponseWriter, r *http.Request) {
	var req domain.PeriodCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.LegalEntityID == "" || req.PeriodName == "" || req.PeriodStart.IsZero() || req.PeriodEnd.IsZero() {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, period_name, period_start, period_end are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCloseConfig); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if !req.PeriodEnd.After(req.PeriodStart) {
		// A period that ends before it begins contains nothing, so every
		// readiness check trivially passes and it locks clean — an empty close
		// over a window that cannot hold a transaction.
		writeError(w, http.StatusBadRequest, "invalid_period_range", "period_end must be after period_start")
		return
	}

	fp := &domain.FiscalPeriod{
		FiscalPeriodID: uuid.NewString(),
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		PeriodName:     req.PeriodName,
		PeriodStart:    req.PeriodStart.UTC(),
		PeriodEnd:      req.PeriodEnd.UTC(),
		CloseStatus:    "OPEN",
	}

	created, err := h.store.CreateFiscalPeriod(r.Context(), fp)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}
	if !created {
		// Replay of a prior request for the same (legal_entity_id, period_name)
		// — return the original period rather than erroring.
		writeJSON(w, http.StatusOK, fp)
		return
	}

	writeJSON(w, http.StatusCreated, fp)
}

// ── GET /v1/close/periods ─────────────────────────────────────────────────────────

func (h *Handler) ListFiscalPeriods(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")

	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Checked here and not left to the store. Every store method does reject an
	// empty tenant, but relying on that puts the boundary one layer further in
	// than the request it guards — and a handler that never states the
	// requirement will not have it when a future read reaches for a different
	// method. Fail closed at the edge.
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCloseView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListFiscalPeriods(r.Context(), legalEntityID)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	// A nil slice marshals to JSON null, which every caller then has to
	// special-case. An entity with no periods registered is an empty list.
	if list == nil {
		list = []domain.FiscalPeriod{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/close/periods/status ──────────────────────────────────────────────────

func (h *Handler) GetPeriodStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID := q.Get("legal_entity_id")
	periodName := q.Get("period_name")

	if legalEntityID == "" || periodName == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id and period_name are required")
		return
	}

	// Deliberately not authorized: this is the endpoint general-ledger-svc calls
	// before every journal create, post and reverse, service to service and with
	// no end-user principal to check. It IS tenant isolated, and the scope is
	// required rather than optional — see below for why that matters more here
	// than anywhere else in this service.
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	fp, err := h.store.GetFiscalPeriodByName(r.Context(), legalEntityID, periodName)
	if errors.Is(err, domain.ErrFiscalPeriodNotFound) {
		// A period nobody registered is open. That is the intended default —
		// this service does not own the calendar, and refusing to post to any
		// period until someone had registered it would make the ledger
		// unusable rather than safe.
		//
		// It is also why the tenant scope above is mandatory. This is the ONE
		// place in the estate that answers a security-relevant question by
		// failing OPEN, and general-ledger-svc fails CLOSED on everything else
		// specifically so that a locked period cannot be posted into. A missing
		// or wrong tenant scope makes any period look unregistered, so without
		// that check the whole period lock could be stepped around by omitting
		// a header — the caller would be told OPEN and the ledger would believe
		// it.
		writeJSON(w, http.StatusOK, map[string]string{
			"period_name":  periodName,
			"close_status": "OPEN",
		})
		return
	}
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"fiscal_period_id": fp.FiscalPeriodID,
		"period_name":      fp.PeriodName,
		"close_status":     fp.CloseStatus,
	})
}

// ── POST /v1/close/periods/{id}/lock ──────────────────────────────────────────────

func (h *Handler) LockPeriod(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = uuid.NewString()
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseInitiate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if fp.CloseStatus != "OPEN" {
		writeError(w, http.StatusUnprocessableEntity, "period_already_locked", string(domain.ErrPeriodAlreadyLocked))
		return
	}

	h.publisher.PublishCloseStarted(r.Context(), correlationID, principalID, *fp)

	// Step 1: Run Readiness Checks (FAIL CLOSED on any dependency query error)
	blockingIssues, err := h.checkReadiness(r.Context(), tenantID, fp)
	if err != nil {
		h.writeReadinessErr(w, err)
		return
	}

	if len(blockingIssues) > 0 {
		h.publisher.PublishCloseBlocked(r.Context(), correlationID, principalID, *fp, blockingIssues)
		h.log.Warn("period close blocked by outstanding items", zap.String("period_id", id), zap.Strings("reasons", blockingIssues))
		writeJSON(w, http.StatusUnprocessableEntity, domain.ReadinessCheckResponse{
			IsReady:        false,
			BlockingIssues: blockingIssues,
		})
		return
	}

	// Step 2: Compile Trial Balance & Generate Evidence (FAIL CLOSED on error)
	balances, err := h.clients.CompileTrialBalance(r.Context(), tenantID, fp.LegalEntityID, fp.PeriodName)
	if err != nil {
		h.log.Error("failed to compile trial balance", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "close_failed", "cannot compile trial balance: "+err.Error())
		return
	}

	// Upload evidence to Document Vault (exposes file UUID)
	docID, err := h.clients.UploadCloseEvidence(r.Context(), tenantID, fp.LegalEntityID, fp.PeriodName, balances, principalID)
	if err != nil {
		h.log.Error("failed to upload close evidence document", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "close_failed", "failed to record close evidence in vault")
		return
	}

	// Calculate verification hash & cryptographic signature
	keys := make([]string, 0, len(balances))
	for k := range balances {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	for _, k := range keys {
		// Two decimal places, not %f's six. The balances are money summed from
		// NUMERIC(18,2) ledger lines, so anything past the second place is
		// float64 noise — and hashing that noise makes the "verification hash"
		// depend on artefacts of binary floating point rather than on the
		// balance an accountant would read.
		buf.WriteString(fmt.Sprintf("%s:%.2f;", k, balances[k]))
	}

	hashBytes := sha256.Sum256(buf.Bytes())
	trialBalanceHash := hex.EncodeToString(hashBytes[:])
	signature := h.signEvidence(hashBytes[:])

	now := time.Now().UTC()

	// Update DB record lock state
	if err := h.store.LockFiscalPeriod(r.Context(), id, now, docID); err != nil {
		if errors.Is(err, domain.ErrPeriodAlreadyLocked) {
			// Replay of a prior request that already succeeded (e.g. a client
			// timeout on a lock call that actually completed server-side) —
			// return the current locked state rather than misreporting this
			// as a store outage.
			current, getErr := h.store.GetFiscalPeriod(r.Context(), id)
			if getErr != nil {
				h.log.Error("failed to fetch already-locked period", zap.Error(getErr))
				writeError(w, http.StatusServiceUnavailable, "store_unavailable", getErr.Error())
				return
			}
			writeJSON(w, http.StatusOK, domain.PeriodLockResponse{
				FiscalPeriodID:     current.FiscalPeriodID,
				PeriodName:         current.PeriodName,
				CloseStatus:        current.CloseStatus,
				CloseLockedAt:      derefTime(current.CloseLockedAt),
				EvidenceDocumentID: derefString(current.EvidenceDocumentID),
			})
			return
		}
		h.log.Error("failed to lock fiscal period", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// Insert close evidence.
	//
	// A failure here used to be logged and swallowed, on the reasoning that the
	// period was already locked. But the evidence row IS the close: the trial
	// balance hash and its signature are the only record of what the books said
	// at the moment they were sealed, and the response below returns a
	// verification_hash that would, in that case, exist nowhere but in this
	// reply. Reporting success would be vouching for evidence that was never
	// written. The period stays locked — it is, and pretending otherwise would
	// be a second lie — and the caller is told plainly that the evidence is
	// missing, which is a condition someone has to act on.
	evidence := &domain.CloseEvidence{
		EvidenceID:       uuid.NewString(),
		TenantID:         tenantID,
		FiscalPeriodID:   id,
		TrialBalanceHash: trialBalanceHash,
		Signature:        signature,
		GeneratedAt:      now,
	}
	if err := h.store.CreateCloseEvidence(r.Context(), evidence); err != nil {
		h.log.Error("period locked but close evidence could not be recorded",
			zap.String("period_id", id), zap.Error(err))
		writeError(w, http.StatusInternalServerError, "evidence_not_recorded",
			string(domain.ErrEvidenceNotRecorded)+
				" — the period IS locked and the trial balance document was uploaded to the vault ("+docID+
				"), but the signed hash was not persisted. Do not treat this close as evidenced.")
		return
	}

	h.publisher.PublishClosed(r.Context(), correlationID, principalID, *fp, docID)

	writeJSON(w, http.StatusOK, domain.PeriodLockResponse{
		FiscalPeriodID:     id,
		PeriodName:         fp.PeriodName,
		CloseStatus:        "LOCKED",
		CloseLockedAt:      now,
		EvidenceDocumentID: docID,
		VerificationHash:   trialBalanceHash,
	})
}

// ── GET /v1/close/periods/{id}/readiness ─────────────────────────────────────────
//
// The same three checks the lock runs, with no side effects: nothing is
// written, nothing is published, and the period is not touched.
//
// Without this the only way to learn whether a period could close was to
// attempt the close — which emits close.started and close.blocked events for
// what was really a question, and leaves an audit trail of attempted closes
// that nobody attempted. A month-end is checked repeatedly and locked once.
func (h *Handler) GetPeriodReadiness(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}

	// Reading readiness is a view, not an initiation — it changes nothing.
	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// A period that is already locked is not "ready to close" — it is closed.
	// Answering is_ready:true would invite a lock that then fails 422.
	if fp.CloseStatus != "OPEN" {
		writeJSON(w, http.StatusOK, domain.ReadinessCheckResponse{
			IsReady:        false,
			BlockingIssues: []string{"period_already_locked: this period is " + fp.CloseStatus + " and cannot be closed again"},
		})
		return
	}

	blockingIssues, err := h.checkReadiness(r.Context(), tenantID, fp)
	if err != nil {
		h.writeReadinessErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, domain.ReadinessCheckResponse{
		IsReady: len(blockingIssues) == 0,
		// Never nil: a JSON null forces every caller to special-case it, and
		// "no blocking issues" is an empty list.
		BlockingIssues: append([]string{}, blockingIssues...),
	})
}

// checkReadiness runs the three dependency checks for a period.
//
// Fails closed: a dependency that cannot be queried returns an error, never an
// empty issue list. "We could not check" and "there is nothing to report" are
// opposite answers, and conflating them would close a period on the strength of
// a service being down.
func (h *Handler) checkReadiness(ctx context.Context, tenantID string, fp *domain.FiscalPeriod) ([]string, error) {
	var issues []string

	unposted, err := h.clients.GetUnpostedJournalsCount(ctx, tenantID, fp.LegalEntityID, fp.PeriodName)
	if err != nil {
		h.log.Error("failed to verify outstanding journals", zap.Error(err))
		return nil, fmt.Errorf("general-ledger-svc: %w", err)
	}
	if unposted > 0 {
		issues = append(issues, fmt.Sprintf("unposted_journals_exist: %d %s in PENDING or VALIDATED status",
			unposted, plural(unposted, "journal is", "journals are")))
	}

	unsettledAP, err := h.clients.GetUnsettledAPInvoicesCount(ctx, tenantID, fp.LegalEntityID, fp.PeriodStart, fp.PeriodEnd)
	if err != nil {
		h.log.Error("failed to verify AP invoices", zap.Error(err))
		return nil, fmt.Errorf("accounts-payable-svc: %w", err)
	}
	if unsettledAP > 0 {
		issues = append(issues, fmt.Sprintf("unsettled_ap_invoices_exist: %d %s due in this period not fully payment requested",
			unsettledAP, plural(unsettledAP, "invoice is", "invoices are")))
	}

	unsettledAR, err := h.clients.GetUnsettledARInvoicesCount(ctx, tenantID, fp.LegalEntityID, fp.PeriodStart, fp.PeriodEnd)
	if err != nil {
		h.log.Error("failed to verify AR invoices", zap.Error(err))
		return nil, fmt.Errorf("accounts-receivable-svc: %w", err)
	}
	if unsettledAR > 0 {
		issues = append(issues, fmt.Sprintf("unsettled_ar_invoices_exist: %d %s due in this period not PAID",
			unsettledAR, plural(unsettledAR, "invoice is", "invoices are")))
	}

	return issues, nil
}

// plural picks the singular or plural wording for a count. These strings are
// rendered verbatim in the console's blocking-issue list, and "1 journals are
// in PENDING status" is the kind of detail that makes a careful message read as
// generated noise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// signEvidence produces the HMAC over the trial balance hash.
//
// The key used to be the tenant ID. A tenant ID is not a secret — it travels in
// the X-Tenant-Id header of every request, sits in the console's URLs, and is
// printed in this service's own responses — so anyone who had ever seen a
// request could forge a signature over any trial balance they liked. A field
// named `signature`, stored beside the hash it covers, states that the evidence
// is attributable and tamper-evident; keyed with a public value it stated
// something untrue, which is worse than storing no signature at all.
//
// The key now comes from CLOSE_SIGNING_KEY and the service refuses to start
// without one (see cmd/server), so this can never silently fall back to
// something guessable.
func (h *Handler) signEvidence(hash []byte) string {
	mac := hmac.New(sha256.New, h.signingKey)
	mac.Write(hash)
	return hex.EncodeToString(mac.Sum(nil))
}

// ── Helpers ──────────────────────────────────────────────────────────────────────

// writeReadinessErr reports a dependency that could not be queried. Always 503
// and always a refusal: a close is never allowed to proceed on an unchecked
// dependency.
func (h *Handler) writeReadinessErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrLedgerPageTruncated) {
		writeError(w, http.StatusServiceUnavailable, "ledger_page_truncated", string(domain.ErrLedgerPageTruncated))
		return
	}
	writeError(w, http.StatusServiceUnavailable, "readiness_check_failed", err.Error()+" — close blocked")
}

// writeStoreErr maps a store failure to the status it actually means. A missing
// tenant scope is the caller's request being wrong (401) and an unknown period
// is a 404; neither is the database being down, and both used to answer 503.
func (h *Handler) writeStoreErr(w http.ResponseWriter, err error, notFoundCode string) {
	switch {
	case errors.Is(err, domain.ErrFiscalPeriodNotFound):
		writeError(w, http.StatusNotFound, notFoundCode, "")
	case errors.Is(err, domain.ErrIdentityMissing), errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
	default:
		h.log.Error("store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// requireTenant reads the caller's verified tenant scope from X-Tenant-Id, set
// by the gateway's ForwardAuth step alongside X-Principal-Id. A request without
// one never passed verification and is refused rather than served under a
// tenant it names itself.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", string(domain.ErrTenantScopeMissing))
		return "", false
	}
	return tenantID, true
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// maxRequestBytes caps a JSON request body. A bare json.Decoder reads until EOF,
// so without this a single request can make the service allocate whatever the
// client is willing to send -- no auth needed, and nothing in the metrics to
// distinguish it from load.
const maxRequestBytes = 256 << 10 // 256 KiB

// decodeJSON reads a size-capped JSON body, answering 413 rather than 400 when
// the cap is what stopped it: "too large" and "malformed" are different faults
// and a caller can only act on the difference.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}
