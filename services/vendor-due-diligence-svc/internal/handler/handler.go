package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	svcmiddleware "zoiko.io/vendor-due-diligence-svc/internal/middleware"
	"zoiko.io/vendor-due-diligence-svc/internal/store"
)

type Store interface {
	CreateCheck(ctx context.Context, c *domain.VendorDDCheck) (created bool, err error)
	GetCheck(ctx context.Context, id string) (*domain.VendorDDCheck, error)
	ListChecks(ctx context.Context, f store.ListFilter) ([]domain.VendorDDCheck, error)
	// ConcludeCheck writes the outcome and its supporting evidence in ONE
	// transaction. It replaced a separate AddEvidence/CompleteCheck pair whose
	// evidence failure was swallowed — see the store method for why that
	// mattered here more than it would elsewhere.
	ConcludeCheck(ctx context.Context, checkID, riskOutcome, screeningBasis, screeningSource string, evidence *domain.VendorDDEvidence) error
	MarkFailed(ctx context.Context, checkID, reason string) error
	ListEvidence(ctx context.Context, checkID string) ([]domain.VendorDDEvidence, error)
}

type Publisher interface {
	PublishStarted(ctx context.Context, correlationID string, check domain.VendorDDCheck)
	PublishCompleted(ctx context.Context, correlationID string, check domain.VendorDDCheck)
	PublishFailed(ctx context.Context, correlationID string, check domain.VendorDDCheck, reason string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// CounterpartyClient pushes a completed check's outcome onto the
// counterparty's record in counterparty-management-svc. Best-effort by
// design — see updateCounterparty below.
type CounterpartyClient interface {
	UpdateComplianceStatus(ctx context.Context, tenantID, counterpartyID, complianceStatus string) error
	UpdateRiskCategory(ctx context.Context, tenantID, counterpartyID, riskCategory string) error
}

const (
	actionInitiate = "VENDOR_DD_INITIATE"
	actionView     = "VENDOR_DD_VIEW"
)

// Pagination bounds for the register. The route previously had none at all, so
// every read returned the tenant's entire screening history.
const (
	defaultLimit = 50
	maxLimit     = 200
)

// stubSanctionsDenylist is a documented stub, not a real sanctions-list
// integration — external-data-feed-svc was checked and does not carry
// sanctions/watchlist data (its FeedType is MARKET_DATA/CREDIT_SCORE/
// COMPANY_INFO/FX_RATE/ESG_DATA only), so a real feed for this doesn't exist
// yet on the platform. This is a case-insensitive exact-name match against a
// hardcoded list, matching the "documented stub-first posture" convention
// used by accounts-payable-svc's authz client. Replace with a real
// sanctions-list integration when one exists.
//
// Because it is exact, "Acme Sanctioned Holdings Ltd" does NOT match — which is
// precisely why every conclusion carries screening_source on the wire. A CLEAR
// from this list is not a sanctions clearance, and a consumer can only avoid
// over-reading it if the record says what actually ran.
var stubSanctionsDenylist = []string{
	"acme sanctioned holdings",
	"restricted trading corp",
}

// screenVendorName runs the stub check. flagged=true means the name matched
// the denylist; basis is always populated so there is a human-readable
// reason on the evidence record either way.
//
// Internal whitespace is collapsed as well as trimmed: "Restricted  Trading Corp"
// with a double space is the same name as far as an exact-match list is
// concerned, and treating it as different would let a stray keystroke defeat the
// only screening there is.
func screenVendorName(vendorName string) (flagged bool, basis string) {
	needle := strings.ToLower(strings.Join(strings.Fields(vendorName), " "))
	for _, entry := range stubSanctionsDenylist {
		if needle == entry {
			return true, "matched stub sanctions denylist entry: " + entry
		}
	}
	return false, "no match against stub sanctions denylist"
}

type Handler struct {
	store        Store
	publisher    Publisher
	authz        AuthZClient
	counterparty CounterpartyClient
	log          *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, counterparty CounterpartyClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, counterparty: counterparty, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/vendor-checks", func(r chi.Router) {
		r.Post("/", h.CreateCheck)
		r.Get("/", h.ListChecks)
		r.Get("/{id}", h.GetCheck)
	})
}

// ── POST /v1/vendor-checks ────────────────────────────────────────────────────

// CreateCheck starts a due-diligence check and runs it to completion
// synchronously (the stub screening has no I/O, so there is nothing to wait
// on asynchronously here — a real screening integration might change that).
//
// Idempotent on (tenant_id, correlation_id): a retry of an already-processed
// request returns the existing check and its evidence as-is, without
// re-running screening or re-notifying counterparty-management-svc a second
// time for the same request.
func (h *Handler) CreateCheck(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Trimmed before the emptiness test, not after. `vendor_name: "   "` passed
	// the old `!= ""` check, then screenVendorName trimmed it to "", matched
	// nothing, and the check concluded CLEAR — a clean due-diligence result for a
	// vendor with no name. Every id field has the same problem in milder form: a
	// padded uuid reaches Postgres and dies in the driver as a 503.
	req.CounterpartyID = strings.TrimSpace(req.CounterpartyID)
	req.LegalEntityID = strings.TrimSpace(req.LegalEntityID)
	req.VendorName = strings.TrimSpace(req.VendorName)
	req.CorrelationID = strings.TrimSpace(req.CorrelationID)
	req.DocumentReference = strings.TrimSpace(req.DocumentReference)

	if req.CounterpartyID == "" || req.LegalEntityID == "" || req.VendorName == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields",
			"counterparty_id, legal_entity_id, vendor_name, correlation_id are required and must not be blank")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// The entity is the authorization scope for this write, so it has to be a UUID
	// for the same reason the list filter does — see validScope.
	if !validScope(w, req.LegalEntityID) {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionInitiate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	correlationID := getCorrelationID(r)
	now := time.Now().UTC()

	check := &domain.VendorDDCheck{
		CheckID:                uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          req.LegalEntityID,
		CounterpartyID:         req.CounterpartyID,
		VendorName:             req.VendorName,
		Status:                 domain.StatusStarted,
		CorrelationID:          req.CorrelationID,
		InitiatedByPrincipalID: principalID,
		StartedAt:              now,
	}

	created, err := h.store.CreateCheck(r.Context(), check)
	if err != nil {
		h.writeStoreErr(w, "failed to create vendor dd check", err)
		return
	}

	if !created {
		h.replayCheck(w, r, check)
		return
	}

	h.publisher.PublishStarted(r.Context(), correlationID, *check)

	flagged, basis := screenVendorName(req.VendorName)
	riskOutcome := domain.RiskClear
	if flagged {
		riskOutcome = domain.RiskFlagged
	}

	evidence := &domain.VendorDDEvidence{
		EvidenceID:        uuid.NewString(),
		CheckID:           check.CheckID,
		TenantID:          tenantID,
		EvidenceType:      domain.EvidenceTypeSanctionsScreening,
		Description:       basis,
		DocumentReference: req.DocumentReference,
		RecordedAt:        time.Now().UTC(),
	}

	// The outcome and the evidence for it land in one transaction or not at all.
	// Previously the evidence write's failure was logged and swallowed and the
	// completion ran regardless, so the response could report COMPLETED/CLEAR
	// alongside an evidence record the store did not hold.
	if err := h.store.ConcludeCheck(
		r.Context(), check.CheckID, riskOutcome, basis, domain.ScreeningSourceStubDenylist, evidence,
	); err != nil {
		h.failCheck(w, r, check, correlationID, err)
		return
	}

	check.Status = domain.StatusCompleted
	check.RiskOutcome = riskOutcome
	check.ScreeningBasis = basis
	check.ScreeningSource = domain.ScreeningSourceStubDenylist
	completedAt := time.Now().UTC()
	check.CompletedAt = &completedAt

	h.publisher.PublishCompleted(r.Context(), correlationID, *check)

	h.updateCounterparty(r.Context(), tenantID, check.CounterpartyID, riskOutcome)

	writeJSON(w, http.StatusCreated, domain.CheckDetailResponse{
		Check:    *check,
		Evidence: []domain.VendorDDEvidence{*evidence},
	})
}

// replayCheck answers a retry of an already-processed correlation_id with what is
// actually on record.
//
// The stored check is returned rather than the request's freshly-built one, and
// Replayed is set so a caller does not have to infer it from the status code. A
// replay can resolve to a check an earlier attempt left in STARTED, which carries
// no risk outcome — reporting that as a fresh 200 with an empty outcome is how a
// lost screening comes to look like a completed one.
func (h *Handler) replayCheck(w http.ResponseWriter, r *http.Request, check *domain.VendorDDCheck) {
	evidence, err := h.store.ListEvidence(r.Context(), check.CheckID)
	if err != nil {
		h.writeStoreErr(w, "failed to list evidence for replayed check", err)
		return
	}
	if evidence == nil {
		evidence = []domain.VendorDDEvidence{}
	}
	writeJSON(w, http.StatusOK, domain.CheckDetailResponse{
		Check:    *check,
		Evidence: evidence,
		Replayed: true,
	})
}

// failCheck records that a screening was attempted and could not be concluded.
//
// This whole path did not exist: FAILED was a status no code could write and
// vendor.dd.failed was declared in the spec with no way to emit it. A check whose
// conclusion failed was left in STARTED forever — indistinguishable in the
// register from a check that had never been screened, with nothing downstream told
// that a screening had been lost.
//
// A concurrent conclusion (ErrCheckAlreadyConcluded) is NOT a failure of the
// check: another request already concluded this row, so the outcome stands and
// marking it FAILED would destroy a valid result. It is reported as a conflict
// against this attempt instead.
//
// Marking FAILED needs the store, so when the store itself is what broke this is
// best-effort and says so in the log rather than pretending. The 503 is honest
// either way: the caller does not have a due-diligence result.
func (h *Handler) failCheck(
	w http.ResponseWriter,
	r *http.Request,
	check *domain.VendorDDCheck,
	correlationID string,
	cause error,
) {
	if errors.Is(cause, domain.ErrCheckAlreadyConcluded) {
		h.log.Warn("vendor dd check already concluded by a concurrent request",
			zap.String("check_id", check.CheckID))
		writeError(w, http.StatusConflict, "check_already_concluded",
			"this check was concluded by another request; read it back rather than re-running it")
		return
	}
	if errors.Is(cause, domain.ErrTenantMissing) {
		h.writeStoreErr(w, "failed to conclude vendor dd check", cause)
		return
	}

	reason := "could not record the screening outcome and its evidence"
	h.log.Error("failed to conclude vendor dd check", zap.String("check_id", check.CheckID), zap.Error(cause))

	if err := h.store.MarkFailed(r.Context(), check.CheckID, reason); err != nil {
		// The row stays STARTED. Logged loudly because it is the one state this
		// service cannot self-describe, and the failed event below is then the
		// only trace that the attempt happened at all.
		h.log.Error("failed to mark vendor dd check FAILED; it remains STARTED",
			zap.String("check_id", check.CheckID), zap.Error(err))
	} else {
		check.Status = domain.StatusFailed
		failedAt := time.Now().UTC()
		check.CompletedAt = &failedAt
	}

	check.ScreeningBasis = reason
	h.publisher.PublishFailed(r.Context(), correlationID, *check, reason)

	writeError(w, http.StatusServiceUnavailable, "store_unavailable",
		"the screening ran but its outcome could not be recorded, so there is no due diligence result")
}

// updateCounterparty pushes this check's outcome onto counterparty-
// management-svc, best-effort. counterparty-management-svc being unreachable
// (or slow, or itself broken) does not make this service's own due-diligence
// result any less true — the check ran, the evidence is recorded, and that
// is the durable source of truth. A failed push here just means the
// counterparty record wasn't enriched with it yet; it is logged loudly so
// the gap is visible, but it never fails the request back to the caller.
func (h *Handler) updateCounterparty(ctx context.Context, tenantID, counterpartyID, riskOutcome string) {
	complianceStatus := "VERIFIED"
	if riskOutcome == domain.RiskFlagged {
		complianceStatus = "REJECTED"
	}

	if err := h.counterparty.UpdateComplianceStatus(ctx, tenantID, counterpartyID, complianceStatus); err != nil {
		h.log.Warn("failed to push compliance status to counterparty-management-svc",
			zap.String("counterparty_id", counterpartyID), zap.Error(err))
	}

	if riskOutcome == domain.RiskFlagged {
		if err := h.counterparty.UpdateRiskCategory(ctx, tenantID, counterpartyID, "HIGH"); err != nil {
			h.log.Warn("failed to push risk category to counterparty-management-svc",
				zap.String("counterparty_id", counterpartyID), zap.Error(err))
		}
	}
}

// ── GET /v1/vendor-checks ─────────────────────────────────────────────────────

func (h *Handler) ListChecks(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	legalEntityID := strings.TrimSpace(r.URL.Query().Get("legal_entity_id"))
	counterpartyID := strings.TrimSpace(r.URL.Query().Get("counterparty_id"))

	// Unconditional now. This used to check authorization ONLY when
	// legal_entity_id was supplied, so omitting the filter skipped the check
	// entirely and returned the tenant's whole screening history — including
	// which vendors were flagged — to any caller holding a tenant header. The
	// unscoped list still has a scope; it is the tenant.
	if !h.authorizeScope(w, r, principalID, legalEntityID, actionView) {
		return
	}

	limit, ok := intParam(w, r, "limit", defaultLimit, 1, maxLimit)
	if !ok {
		return
	}
	offset, ok := intParam(w, r, "offset", 0, 0, 0)
	if !ok {
		return
	}

	list, err := h.store.ListChecks(r.Context(), store.ListFilter{
		LegalEntityID:  legalEntityID,
		CounterpartyID: counterpartyID,
		Limit:          limit,
		Offset:         offset,
	})
	if err != nil {
		// No invalid-identifier branch here on purpose. Unlike check_id, this
		// table's legal_entity_id and counterparty_id are VARCHAR(255) and not
		// uuid columns, so a malformed filter is a valid comparison that simply
		// matches nothing — it never reaches the driver as SQLSTATE 22P02. A 400
		// here would claim a validation this schema does not perform.
		h.writeStoreErr(w, "failed to list vendor dd checks", err)
		return
	}
	if list == nil {
		list = []domain.VendorDDCheck{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/vendor-checks/{id} ────────────────────────────────────────────────

func (h *Handler) GetCheck(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// Read first, authorize second: the entity to authorize against is on the
	// record. Row-level security scopes the read to this tenant, so another
	// tenant's check reads as absent here rather than reaching the authz call.
	check, err := h.store.GetCheck(r.Context(), id)
	if errors.Is(err, domain.ErrCheckNotFound) {
		writeError(w, http.StatusNotFound, "check_not_found", "")
		return
	}
	if err != nil {
		h.writeStoreErr(w, "failed to fetch vendor dd check", err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, check.LegalEntityID, actionView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	evidence, err := h.store.ListEvidence(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, "failed to list vendor dd evidence", err)
		return
	}
	if evidence == nil {
		evidence = []domain.VendorDDEvidence{}
	}

	writeJSON(w, http.StatusOK, domain.CheckDetailResponse{Check: *check, Evidence: evidence})
}

// ── Helpers ────────────────────────────────────────────────────────────────

// authorizeScope checks the caller against a legal entity, falling back to the
// tenant when no entity was named. The fallback is what makes the check
// unconditional. Returns false when it has already written the response.
func (h *Handler) authorizeScope(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, action string) bool {
	scope := legalEntityID
	if scope == "" {
		scope = svcmiddleware.TenantFromContext(r.Context())
	}
	if scope == "" {
		// No entity and no tenant: nothing to authorize against, so there is no
		// way to establish the caller may read anything. Fail closed.
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrTenantMissing))
		return false
	}
	if !validScope(w, scope) {
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, action); err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

// validScope refuses an authorization scope that cannot be one, before asking
// authorization-svc about it.
//
// This exists because of how authorization-svc fails. It stores legal_entity_id in
// a uuid column and answers **503 `store_unavailable`** for a non-UUID value — its
// own instance of the platform-wide habit of reporting a driver error as an outage.
// From here that 503 is indistinguishable from authorization-svc genuinely being
// down, so a caller who mistyped a filter was told the authorization plane had
// failed. Nothing downstream could tell the difference either.
//
// The check therefore has to happen on this side. Note the scope requirement comes
// from authorization-svc and not from this service's own schema: these columns are
// VARCHAR here, so a malformed *counterparty* filter is fine and simply matches
// nothing. It is specifically the value used as the authorization scope that must
// be a UUID.
//
// Returns false when it has already written the response.
func validScope(w http.ResponseWriter, scope string) bool {
	if _, err := uuid.Parse(scope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_scope",
			"legal_entity_id must be a UUID: it is used as the authorization scope, "+
				"and authorization-svc reports a malformed one as an outage rather than a bad request")
		return false
	}
	return true
}

// writeStoreErr keeps a missing tenant scope apart from a broken database.
//
// Every store method rejects a request carrying no tenant scope, and all of those
// rejections used to be reported as 503 `store_unavailable` — so a caller who had
// simply omitted X-Tenant-Id was told this service's database was down. The raw
// error text is no longer echoed either: it carried driver, DSN, and query detail
// to the client for no one's benefit.
func (h *Handler) writeStoreErr(w http.ResponseWriter, msg string, err error) {
	if errors.Is(err, domain.ErrTenantMissing) || errors.Is(err, domain.ErrIdentityMissing) {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrTenantMissing))
		return
	}
	h.log.Error(msg, zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
}

// intParam reads a bounded integer query parameter.
//
// strconv.Atoi's error was previously discarded across this platform, so
// `limit=abc` silently became the default and `offset=-1` reached Postgres and
// answered 503. A parameter the caller got wrong is reported to them.
// max <= 0 means unbounded above.
func intParam(w http.ResponseWriter, r *http.Request, name string, def, min, max int) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_"+name, name+" must be an integer")
		return 0, false
	}
	if n < min {
		writeError(w, http.StatusBadRequest, "invalid_"+name,
			fmt.Sprintf("%s must be at least %d", name, min))
		return 0, false
	}
	if max > 0 && n > max {
		writeError(w, http.StatusBadRequest, "invalid_"+name,
			fmt.Sprintf("%s must be at most %d", name, max))
		return 0, false
	}
	return n, true
}

// maxRequestBytes caps a request body. A vendor check is a few hundred bytes;
// 64 KiB is already generous, and without a cap a single caller can stream an
// unbounded body into the decoder.
const maxRequestBytes = 64 << 10

// decodeJSON reads a JSON body strictly, and reports why it was refused.
//
// DisallowUnknownFields matters most for `document_reference`: it is optional, so
// a misspelling would be silently discarded and the check would conclude with its
// evidence pointing at no document — with nothing downstream able to tell that
// from a caller who never sent one. Accepting a body and ignoring part of it is
// worse than rejecting it.
//
// Returns false when it has already written the response.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("body exceeds %d bytes", maxRequestBytes))
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			writeError(w, http.StatusBadRequest, "unknown_field", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		}
		return false
	}
	return true
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

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

// writeError emits the platform's error shape: `error` for the machine code,
// `detail` for the human part.
//
// This service used to answer `{"error_code":…,"error_message":…}`. Every other
// service uses `error`/`detail`, and the admin console parses exactly those keys
// (plus `field` and `message`), so every failure from this service would have
// arrived in the UI as a bare status code with no explanation whatsoever. Nothing
// consumed the old shape — no other backend service calls this one, and the
// console had no client for it at all.
func writeError(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]string{"error": code}
	if detail != "" {
		body["detail"] = detail
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
