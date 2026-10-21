// Package source reads reconciliation populations from their owning services.
//
// This is a copy of financial-control-svc's internal/source client, adapted to
// this service's domain types. Services here share no Go module (see
// services/_contract/rollout.sh: shared code is vendored, never imported), so
// the client is duplicated rather than extracted. Keep the two in step when the
// contract changes.
//
// The population contract every source service implements (ZS-CONTROL-001 §9,
// docs/architecture/control-population-contract.md):
//
//	GET {base}/v1/control-populations/{population}
//	    ?legal_entity_id=&period_id=&limit=&cursor=
//	200 { "records": [ {record_id, reference, amount, currency, date, attributes} ],
//	      "next_cursor": "",            // empty on the last page
//	      "watermark": "...",           // commit sequence / dataset version; REQUIRED
//	      "declared_totals": { "row_count": n, "totals": {"USD": "123.45"} } }  // REQUIRED, same on every page
//
// The client fails CLOSED. A population that cannot be proven complete and
// reproducible is never returned, so no run is ever started on it.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

var (
	ErrSourceUnavailable  = errors.New("source population unavailable")
	ErrSourceUnknown      = errors.New("source system is not registered")
	ErrSourceInconsistent = errors.New("source population is not reproducible")
	ErrPopulationTooLarge = errors.New("source population exceeds the configured maximum")
)

const (
	pageSize       = 1000
	maxRecords     = 200000
	maxPagesGuard  = maxRecords/pageSize + 5
	maxResponseLen = 32 << 20
)

// Scope is what a fetch is made on behalf of. Tenant and principal are the
// VERIFIED values from the calling request, forwarded so the source service
// applies its own authorization and tenant isolation.
type Scope struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
	PeriodID      string
	CorrelationID string
}

// Fetched is a fully assembled population whose declared totals have been
// verified against the records received.
type Fetched struct {
	Records          []domain.PopulationRecord
	Watermark        string
	DeclaredRowCount int64
	DeclaredTotals   map[string]string
}

// Fetcher builds a population from a source.
type Fetcher interface {
	Fetch(ctx context.Context, spec domain.PopulationSpec, scope Scope) (*Fetched, error)
}

// HTTPFetcher implements Fetcher over the contract above. Base URLs come only
// from server configuration; a definition can select a registered system by
// name but can never supply a URL (no SSRF surface).
type HTTPFetcher struct {
	endpoints map[string]string
	http      *http.Client
}

// ParseEndpoints parses "ar=http://ar:8101,gl=http://gl:8098".
func ParseEndpoints(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("invalid source endpoint %q (want name=url)", part)
		}
		u, err := url.Parse(strings.TrimSpace(v))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("invalid source endpoint url for %q", k)
		}
		out[strings.TrimSpace(k)] = strings.TrimRight(u.String(), "/")
	}
	return out, nil
}

func NewHTTPFetcher(endpoints map[string]string, client *http.Client) *HTTPFetcher {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &HTTPFetcher{endpoints: endpoints, http: client}
}

type declaredTotals struct {
	RowCount int               `json:"row_count"`
	Totals   map[string]string `json:"totals"`
}

type pageResponse struct {
	Records        []domain.PopulationRecord `json:"records"`
	NextCursor     string                    `json:"next_cursor"`
	Watermark      string                    `json:"watermark"`
	DeclaredTotals *declaredTotals           `json:"declared_totals"`
}

func (f *HTTPFetcher) Fetch(ctx context.Context, spec domain.PopulationSpec, scope Scope) (*Fetched, error) {
	base, ok := f.endpoints[spec.System]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSourceUnknown, spec.System)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	var all []domain.PopulationRecord
	var watermark string
	var declared *declaredTotals
	cursor := ""
	for page := 0; ; page++ {
		if page > maxPagesGuard {
			return nil, ErrPopulationTooLarge
		}
		resp, err := f.page(ctx, base, spec, scope, cursor)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(resp.Watermark) == "" {
			// ZS-CONTROL-001 Invariant 3: no watermark, no reproducible population.
			return nil, fmt.Errorf("%w: %s returned no watermark", ErrSourceInconsistent, spec.Ref())
		}
		if page == 0 {
			watermark = resp.Watermark
		} else if resp.Watermark != watermark {
			// The source moved while we were reading it: what we hold is a blend of two
			// states and would not be reproducible.
			return nil, fmt.Errorf("%w: %s watermark changed mid-extraction (%s -> %s)", ErrSourceInconsistent, spec.Ref(), watermark, resp.Watermark)
		}
		for _, r := range resp.Records {
			if err := domain.ValidateRecord(r); err != nil {
				return nil, err
			}
		}
		all = append(all, resp.Records...)
		if len(all) > maxRecords {
			return nil, ErrPopulationTooLarge
		}
		// declared_totals describe the WHOLE population, so they are required on every
		// page and cannot differ between pages; without them a short or altered
		// transfer could not be told apart from the complete one.
		if resp.DeclaredTotals == nil {
			return nil, fmt.Errorf("%w: %s returned no declared_totals", ErrSourceInconsistent, spec.Ref())
		}
		if page == 0 {
			declared = resp.DeclaredTotals
		} else if !sameDeclared(declared, resp.DeclaredTotals) {
			return nil, fmt.Errorf("%w: %s declared_totals changed mid-extraction", ErrSourceInconsistent, spec.Ref())
		}
		if resp.NextCursor == "" {
			break
		}
		if resp.NextCursor == cursor {
			return nil, fmt.Errorf("%w: %s cursor did not advance", ErrSourceInconsistent, spec.Ref())
		}
		cursor = resp.NextCursor
	}

	if err := verifyDeclared(spec, all, declared); err != nil {
		return nil, err
	}
	return &Fetched{Records: all, Watermark: watermark, DeclaredRowCount: int64(declared.RowCount), DeclaredTotals: declared.Totals}, nil
}

// sameDeclared compares two pages' declared totals by row count and exact
// per-currency value (so "6" and "6.00" agree).
func sameDeclared(a, b *declaredTotals) bool {
	if a.RowCount != b.RowCount || len(a.Totals) != len(b.Totals) {
		return false
	}
	for c, v := range a.Totals {
		w, ok := b.Totals[c]
		if !ok || !sameDecimal(v, w) {
			return false
		}
	}
	return true
}

// verifyDeclared checks the source's own control totals against what was
// actually received. A truncated or altered transfer therefore cannot
// masquerade as the whole population.
func verifyDeclared(spec domain.PopulationSpec, recs []domain.PopulationRecord, d *declaredTotals) error {
	if d.RowCount != len(recs) {
		return fmt.Errorf("%w: %s declared %d rows but %d were received", ErrSourceInconsistent, spec.Ref(), d.RowCount, len(recs))
	}
	got, err := domain.DecimalTotals(recs)
	if err != nil {
		return err
	}
	for ccy, total := range got {
		want, ok := d.Totals[ccy]
		w, okw := rat(want)
		if !ok || !okw || w.Cmp(total) != 0 {
			return fmt.Errorf("%w: %s %s total is %s, source declared %q", ErrSourceInconsistent, spec.Ref(), ccy, total.FloatString(12), want)
		}
	}
	for c, v := range d.Totals {
		if _, seen := got[c]; !seen && !sameDecimal(v, "0") {
			return fmt.Errorf("%w: %s declared a %s total of %s with no records", ErrSourceInconsistent, spec.Ref(), c, v)
		}
	}
	return nil
}

func rat(s string) (*big.Rat, bool) {
	return new(big.Rat).SetString(strings.TrimSpace(s))
}

func sameDecimal(a, b string) bool {
	x, ok1 := rat(a)
	y, ok2 := rat(b)
	return ok1 && ok2 && x.Cmp(y) == 0
}

func (f *HTTPFetcher) page(ctx context.Context, base string, spec domain.PopulationSpec, scope Scope, cursor string) (*pageResponse, error) {
	q := url.Values{}
	q.Set("legal_entity_id", scope.LegalEntityID)
	if scope.PeriodID != "" && !spec.NoPeriod {
		q.Set("period_id", scope.PeriodID)
	}
	q.Set("limit", strconv.Itoa(pageSize))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	for k, v := range spec.Params { // validated by spec.Validate; re-checked so a hand-built spec cannot bypass it
		if domain.ValidateParams(map[string]string{k: v}) != nil {
			return nil, fmt.Errorf("%w: invalid population parameter %q", domain.ErrInvalidArgument, k)
		}
		q.Set(k, v)
	}
	u := base + "/v1/control-populations/" + url.PathEscape(spec.Population) + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-Id", scope.TenantID)
	req.Header.Set("X-Principal-Id", scope.PrincipalID)
	req.Header.Set("X-Legal-Entity-Id", scope.LegalEntityID)
	req.Header.Set("X-Correlation-ID", scope.CorrelationID)

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrSourceUnavailable, spec.Ref(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024)) //nolint:errcheck
		return nil, fmt.Errorf("%w: %s answered HTTP %d", ErrSourceUnavailable, spec.Ref(), resp.StatusCode)
	}
	var out pageResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseLen))
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: %s returned an unreadable page: %v", ErrSourceInconsistent, spec.Ref(), err)
	}
	return &out, nil
}

var scopeRefRe = regexp.MustCompile(`^\$\{scope\.([a-z_]{1,32})\}$`)

// ResolveScopeParams binds population params of the form "${scope.<key>}" to
// values from the RUN's scope (e.g. the bank account a run is about). A
// definition stays static and reviewable; the per-run value comes from the
// run, which the caller was authorized to start. A missing key fails closed,
// and the bound value passes the same validation as any literal param.
func ResolveScopeParams(spec domain.PopulationSpec, runScope map[string]string) (domain.PopulationSpec, error) {
	if len(spec.Params) == 0 {
		return spec, nil
	}
	out := spec
	out.Params = make(map[string]string, len(spec.Params))
	for k, v := range spec.Params {
		m := scopeRefRe.FindStringSubmatch(v)
		if m == nil {
			out.Params[k] = v
			continue
		}
		s, ok := runScope[m[1]]
		if !ok || strings.TrimSpace(s) == "" {
			return spec, fmt.Errorf("%w: param %q needs run scope %q, which the run does not carry", domain.ErrInvalidArgument, k, m[1])
		}
		out.Params[k] = strings.TrimSpace(s)
	}
	if err := domain.ValidateParams(out.Params); err != nil {
		return spec, err
	}
	return out, nil
}

// PopulationError is a population that was attempted and could not be proven
// complete and reproducible. It is a finding about the run — the run is
// recorded Failed / INDETERMINATE — unlike an error in the request itself (a
// scope value the run does not carry), which starts nothing.
type PopulationError struct {
	Side string
	Err  error
}

func (e *PopulationError) Error() string { return fmt.Sprintf("population %q: %v", e.Side, e.Err) }
func (e *PopulationError) Unwrap() error { return e.Err }

// Code is the run failure code for this error.
func (e *PopulationError) Code() domain.FailureCode {
	switch {
	case errors.Is(e.Err, ErrSourceUnavailable):
		return domain.FailureSourceUnavailable
	case errors.Is(e.Err, ErrSourceUnknown):
		return domain.FailureSourceUnregistered
	case errors.Is(e.Err, ErrPopulationTooLarge):
		return domain.FailurePopulationTooLarge
	case errors.Is(e.Err, ErrSourceInconsistent):
		return domain.FailureSourceInconsistent
	default:
		// An invalid record, an amount the definition's scale cannot represent,
		// an overflowing total, a duplicate record id.
		return domain.FailurePopulationInvalid
	}
}

// BuildPopulations reads both of a definition's populations for one run and
// converts them to frozen items at the definition's amount scale. Any failure
// (an unreachable or unregistered source, a missing or moving watermark,
// absent or untied declared totals, an invalid record, an amount the scale
// cannot represent exactly) returns a *PopulationError and nothing is frozen.
func BuildPopulations(ctx context.Context, f Fetcher, def domain.ReconciliationDefinition, req domain.StartRunRequest,
	scope Scope) ([]domain.FrozenPopulation, error) {
	if err := domain.ValidatePopulationSpecs(def.Populations); err != nil {
		return nil, err
	}
	out := make([]domain.FrozenPopulation, 0, len(def.Populations))
	for _, spec := range def.Populations {
		bound, err := ResolveScopeParams(spec, req.Scope)
		if err != nil {
			return nil, err
		}
		got, err := f.Fetch(ctx, bound, scope)
		if err != nil {
			return nil, &PopulationError{Side: spec.Side, Err: err}
		}
		fp, err := domain.NewFrozenPopulation(bound, got.Watermark, got.DeclaredRowCount, got.DeclaredTotals, got.Records, def.AmountScale)
		if err != nil {
			return nil, &PopulationError{Side: spec.Side, Err: err}
		}
		out = append(out, fp)
	}
	return out, nil
}
