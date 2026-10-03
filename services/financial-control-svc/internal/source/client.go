// Package source builds control populations from authoritative source services.
//
// The population contract every source service implements (ZS-CONTROL-001 §9):
//
//	GET {base}/v1/control-populations/{population}
//	    ?legal_entity_id=&period_id=&limit=&cursor=
//	200 { "records": [ {record_id, reference, amount, currency, date, attributes} ],
//	      "next_cursor": "",            // empty on the last page
//	      "watermark": "...",           // commit sequence / dataset version; REQUIRED
//	      "declared_totals": { "row_count": n, "totals": {"USD": "123.45"} } }  // optional
//
// The builder fails CLOSED. A population that cannot be proven complete and
// reproducible is never returned — the caller records the run as Indeterminate
// (scenario 09), not as a Pass.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"zoiko.io/financial-control-svc/internal/domain"
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

var populationNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Spec identifies a population inside a control definition's source_spec /
// target_spec: {"system": "accounts-receivable", "population": "invoices"}.
type Spec struct {
	System     string            `json:"system"`
	Population string            `json:"population"`
	Params     map[string]string `json:"params,omitempty"`
	// NoPeriod: the population is not period-scoped (lifetime, or scoped by its own explicit
	// param such as fiscal_period / run_id / count_id) and its source REJECTS period_id as an
	// unknown parameter. The run's period is then not sent.
	NoPeriod bool `json:"no_period,omitempty"`
}

func (s Spec) Ref() string {
	ref := s.System + "/" + s.Population
	if len(s.Params) == 0 {
		return ref
	}
	keys := make([]string, 0, len(s.Params))
	for k := range s.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + s.Params[k]
	}
	return ref + "?" + strings.Join(parts, "&")
}

// ParseSpec reads and validates a spec document.
func ParseSpec(raw json.RawMessage) (Spec, error) {
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("%w: spec is not valid JSON", domain.ErrInvalidArgument)
	}
	if s.System == "" || !populationNameRe.MatchString(s.Population) {
		return s, fmt.Errorf("%w: spec needs a system and a population name (a-z, 0-9, _ or -)", domain.ErrInvalidArgument)
	}
	if err := validateParams(s.Params); err != nil {
		return s, err
	}
	return s, nil
}

// Scope is what a fetch is made on behalf of. Tenant and principal are the
// VERIFIED values from the calling request, forwarded so the source service
// applies its own authorization and tenant isolation (scenario 28).
type Scope struct {
	TenantID      string
	PrincipalID   string
	LegalEntityID string
	PeriodID      string
	CorrelationID string
}

// Fetched is a fully assembled, internally verified population.
type Fetched struct {
	Records   []domain.PopulationRecord
	Watermark string
}

// Fetcher builds a population from a source.
type Fetcher interface {
	Fetch(ctx context.Context, spec Spec, scope Scope) (*Fetched, error)
}

// HTTPFetcher implements Fetcher over the contract above. Base URLs come only
// from server configuration; a request can select a registered system by name
// but can never supply a URL (no SSRF surface).
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

func (f *HTTPFetcher) Fetch(ctx context.Context, spec Spec, scope Scope) (*Fetched, error) {
	base, ok := f.endpoints[spec.System]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSourceUnknown, spec.System)
	}
	if !populationNameRe.MatchString(spec.Population) {
		return nil, fmt.Errorf("%w: population name", domain.ErrInvalidArgument)
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
			// Invariant 3: no watermark, no reproducible population.
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
		if resp.DeclaredTotals != nil {
			declared = resp.DeclaredTotals
		}
		if resp.NextCursor == "" {
			break
		}
		if resp.NextCursor == cursor {
			return nil, fmt.Errorf("%w: %s cursor did not advance", ErrSourceInconsistent, spec.Ref())
		}
		cursor = resp.NextCursor
	}

	if declared != nil {
		if err := verifyDeclared(spec, all, declared); err != nil {
			return nil, err
		}
	}
	return &Fetched{Records: all, Watermark: watermark}, nil
}

// verifyDeclared checks the source's own control totals against what was
// actually received (FIN-CTRL-033: source count/hash to ingest). A truncated or
// altered transfer therefore cannot masquerade as the whole population.
func verifyDeclared(spec Spec, recs []domain.PopulationRecord, d *declaredTotals) error {
	if d.RowCount != len(recs) {
		return fmt.Errorf("%w: %s declared %d rows but %d were received", ErrSourceInconsistent, spec.Ref(), d.RowCount, len(recs))
	}
	got, err := domain.ComputeTotals(recs)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, t := range got {
		seen[t.Currency] = true
		want, ok := d.Totals[t.Currency]
		if !ok || !sameDecimal(want, t.Total) {
			return fmt.Errorf("%w: %s %s total is %s, source declared %q", ErrSourceInconsistent, spec.Ref(), t.Currency, t.Total, want)
		}
	}
	for c, v := range d.Totals {
		if !seen[c] && !sameDecimal(v, "0") {
			return fmt.Errorf("%w: %s declared a %s total of %s with no records", ErrSourceInconsistent, spec.Ref(), c, v)
		}
	}
	return nil
}

func sameDecimal(a, b string) bool {
	x, ok1 := domainRat(a)
	y, ok2 := domainRat(b)
	return ok1 && ok2 && x.Cmp(y) == 0
}

func (f *HTTPFetcher) page(ctx context.Context, base string, spec Spec, scope Scope, cursor string) (*pageResponse, error) {
	q := url.Values{}
	q.Set("legal_entity_id", scope.LegalEntityID)
	if scope.PeriodID != "" && !spec.NoPeriod {
		q.Set("period_id", scope.PeriodID)
	}
	q.Set("limit", strconv.Itoa(pageSize))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	for k, v := range spec.Params { // already validated by ParseSpec; re-checked here so a hand-built Spec cannot bypass it
		if validateParams(map[string]string{k: v}) != nil {
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

var paramKeyRe = regexp.MustCompile(`^[a-z_]{1,32}$`)

// reserved names are set by the client itself; a spec can never override them
// (that would let a definition retarget another entity or period).
var reservedParams = map[string]bool{"legal_entity_id": true, "period_id": true, "limit": true, "cursor": true}

const maxParams, maxParamValue = 8, 512

func validateParams(p map[string]string) error {
	if len(p) > maxParams {
		return fmt.Errorf("%w: at most %d population params", domain.ErrInvalidArgument, maxParams)
	}
	for k, v := range p {
		if !paramKeyRe.MatchString(k) || reservedParams[k] {
			return fmt.Errorf("%w: invalid population param name %q", domain.ErrInvalidArgument, k)
		}
		if len(v) > maxParamValue || strings.ContainsAny(v, "\x00\r\n") {
			return fmt.Errorf("%w: invalid value for population param %q", domain.ErrInvalidArgument, k)
		}
	}
	return nil
}

var scopeRefRe = regexp.MustCompile(`^\$\{scope\.([a-z_]{1,32})\}$`)

// ResolveScopeParams binds population params of the form "${scope.<key>}" to
// values from the RUN's scope (e.g. the stock count a run is about). A control
// definition stays static and reviewable; the per-run value comes from the run,
// which the caller was authorized to create. A missing key fails closed, and the
// bound value passes the same validation as any literal param.
func ResolveScopeParams(spec Spec, runScope json.RawMessage) (Spec, error) {
	if len(spec.Params) == 0 {
		return spec, nil
	}
	var scope map[string]any
	if len(runScope) > 0 {
		if err := json.Unmarshal(runScope, &scope); err != nil {
			return spec, fmt.Errorf("%w: run scope is not a JSON object", domain.ErrInvalidArgument)
		}
	}
	out := spec
	out.Params = make(map[string]string, len(spec.Params))
	for k, v := range spec.Params {
		m := scopeRefRe.FindStringSubmatch(v)
		if m == nil {
			out.Params[k] = v
			continue
		}
		raw, ok := scope[m[1]]
		if !ok {
			return spec, fmt.Errorf("%w: param %q needs run scope %q, which the run does not carry", domain.ErrInvalidArgument, k, m[1])
		}
		s, ok := raw.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return spec, fmt.Errorf("%w: run scope %q must be a non-empty string", domain.ErrInvalidArgument, m[1])
		}
		out.Params[k] = strings.TrimSpace(s)
	}
	if err := validateParams(out.Params); err != nil {
		return spec, err
	}
	return out, nil
}
