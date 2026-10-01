package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/payee-banking-identity-svc/internal/authz"
	"zoiko.io/payee-banking-identity-svc/internal/domain"
	svcmiddleware "zoiko.io/payee-banking-identity-svc/internal/middleware"
)

// ZS-CONTROL-001 §9 control population endpoint — see
// docs/architecture/control-population-contract.md ("payee-banking-identity-svc
// — Wave 6").

const (
	// PayeeBankingControlPopulationRead is the action the verified human caller
	// needs for the entity to read this population.
	PayeeBankingControlPopulationRead = "PAYEE_BANKING_CONTROL_POPULATION_READ"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
	maxChangeSpanDays      = 400
)

var destinationChangesParams = map[string]bool{
	"legal_entity_id": true,
	"changed_from":    true,
	"changed_to":      true,
	"limit":           true,
	"cursor":          true,
}

var legalEntityIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

type controlPopulationResponse struct {
	Records        []domain.ControlPopulationRecord `json:"records"`
	NextCursor     string                           `json:"next_cursor"`
	Watermark      string                           `json:"watermark"`
	DeclaredTotals domain.ControlDeclaredTotals     `json:"declared_totals"`
}

func writePopulationError(w http.ResponseWriter, status int, msg string) {
	writeError(w, status, msg)
}

func parseUTCDate(raw string) (time.Time, bool) {
	if len(raw) != len("2006-01-02") {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", raw)
	return t, err == nil
}

// GetDestinationChangesPopulation serves
// GET /v1/control-populations/destination-changes.
func (h *Handler) GetDestinationChangesPopulation(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if _, err := uuid.Parse(tenantID); err != nil {
		writeError(w, http.StatusUnauthorized, "a valid X-Tenant-Id header is required")
		return
	}

	q := r.URL.Query()
	for k, vs := range q {
		if !destinationChangesParams[k] {
			writePopulationError(w, http.StatusBadRequest, "unknown parameter: "+k)
			return
		}
		if len(vs) != 1 {
			writePopulationError(w, http.StatusBadRequest, k+" must be given at most once")
			return
		}
	}

	entity := q.Get("legal_entity_id")
	if !legalEntityIDPattern.MatchString(entity) {
		writePopulationError(w, http.StatusBadRequest, "legal_entity_id is required (1-64 characters: letters, digits, _ . : -)")
		return
	}
	from, okFrom := parseUTCDate(q.Get("changed_from"))
	to, okTo := parseUTCDate(q.Get("changed_to"))
	if !okFrom || !okTo {
		writePopulationError(w, http.StatusBadRequest, "changed_from and changed_to are required (YYYY-MM-DD)")
		return
	}
	if from.After(to) {
		writePopulationError(w, http.StatusBadRequest, "changed_from must not be after changed_to")
		return
	}
	if int(to.Sub(from).Hours()/24) > maxChangeSpanDays {
		writePopulationError(w, http.StatusBadRequest, "the changed_from..changed_to span must not exceed 400 days")
		return
	}

	limit := defaultPopulationLimit
	if raw := q.Get("limit"); q.Has("limit") {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writePopulationError(w, http.StatusBadRequest, "limit must be an integer between 1 and "+strconv.Itoa(maxPopulationLimit))
			return
		}
		limit = n
	}

	after := ""
	if q.Has("cursor") {
		b, err := base64.RawURLEncoding.DecodeString(q.Get("cursor"))
		if err == nil {
			_, err = uuid.Parse(string(b))
		}
		if err != nil {
			writePopulationError(w, http.StatusBadRequest, "cursor is not valid")
			return
		}
		after = strings.ToLower(string(b))
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, PayeeBankingControlPopulationRead); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
			return
		}
		h.log.Error("control population: authorization check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		return
	}

	page, err := h.store.QueryDestinationChanges(r.Context(), tenantID, domain.DestinationChangesQuery{
		LegalEntityID: entity,
		ChangedFrom:   from.Format("2006-01-02"),
		ChangedTo:     to.Format("2006-01-02"),
		Limit:         limit,
		AfterRecordID: after,
	})
	if err != nil {
		if errors.Is(err, domain.ErrControlPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		h.log.Error("GetDestinationChangesPopulation: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}

	resp := controlPopulationResponse{
		Records:        page.Records,
		Watermark:      page.Watermark,
		DeclaredTotals: page.DeclaredTotals,
	}
	if resp.Records == nil {
		resp.Records = []domain.ControlPopulationRecord{}
	}
	if resp.DeclaredTotals.Totals == nil {
		resp.DeclaredTotals.Totals = map[string]string{}
	}
	if page.NextRecordID != "" {
		resp.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.NextRecordID))
	}
	writeJSON(w, http.StatusOK, resp)
}
