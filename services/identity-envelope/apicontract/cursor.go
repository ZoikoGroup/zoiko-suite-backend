package apicontract

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

var (
	ErrCursorInvalid = errors.New("apicontract: cursor is malformed or its signature does not verify")
	ErrCursorScope   = errors.New("apicontract: cursor was issued for a different query")
)

type SortTerm struct {
	Field string
	Desc  bool
}

type Page struct {
	PageSize int
	Sort     []SortTerm
	Filters  map[string]string
	Cursor   string
}

func (p Page) FilterKey() string {
	keys := make([]string, 0, len(p.Filters))
	for k := range p.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(p.Filters[k])
	}
	return b.String()
}

func (p Page) SortKey() string {
	parts := make([]string, 0, len(p.Sort))
	for _, t := range p.Sort {
		if t.Desc {
			parts = append(parts, "-"+t.Field)
		} else {
			parts = append(parts, t.Field)
		}
	}
	return strings.Join(parts, ",")
}

var filterOperators = map[string]bool{
	"eq": true, "ne": true, "gt": true, "gte": true, "lt": true, "lte": true, "in": true,
}

func ParsePage(r *http.Request, allowedFilters, allowedSort []string, defaultSort string) (Page, *Problem) {
	p := Page{PageSize: DefaultPageSize, Filters: map[string]string{}}
	q := r.URL.Query()

	filterSet := make(map[string]bool, len(allowedFilters))
	for _, f := range allowedFilters {
		filterSet[f] = true
	}
	for key, values := range q {
		if !strings.HasPrefix(key, "filter[") || !strings.HasSuffix(key, "]") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(key, "filter["), "]")
		field, op := inner, "eq"
		if i := strings.Index(inner, "]["); i >= 0 {
			field = inner[:i]
			op = inner[i+2:]
			if !filterOperators[op] {
				prob := ProblemFor(http.StatusBadRequest, "invalid_filter", fmt.Sprintf("filter operator %q is not supported", op))
				return Page{}, &prob
			}
		}
		if !filterSet[field] {
			prob := ProblemFor(http.StatusBadRequest, "unknown_filter", fmt.Sprintf("filter[%s] is not supported by this endpoint", field))
			return Page{}, &prob
		}
		if len(values) > 1 {
			prob := ProblemFor(http.StatusBadRequest, "invalid_filter", fmt.Sprintf("filter[%s] was supplied more than once", inner))
			return Page{}, &prob
		}
		canonical := field
		if op != "eq" {
			canonical = field + "[" + op + "]"
		}
		p.Filters[canonical] = values[0]
	}

	if raw := q.Get("page_size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxPageSize {
			prob := ProblemFor(http.StatusBadRequest, "invalid_page_size", fmt.Sprintf("page_size must be between 1 and %d", MaxPageSize))
			return Page{}, &prob
		}
		p.PageSize = n
	}

	sortSet := make(map[string]bool, len(allowedSort))
	for _, f := range allowedSort {
		sortSet[f] = true
	}
	rawSort := q.Get("sort")
	if rawSort == "" {
		rawSort = defaultSort
	}
	if rawSort != "" {
		for _, part := range strings.Split(rawSort, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			term := SortTerm{Field: strings.TrimPrefix(part, "-"), Desc: strings.HasPrefix(part, "-")}
			if !sortSet[term.Field] {
				prob := ProblemFor(http.StatusBadRequest, "invalid_sort", fmt.Sprintf("sort field %q is not supported by this endpoint", term.Field))
				return Page{}, &prob
			}
			p.Sort = append(p.Sort, term)
		}
	}
	if sortSet["id"] {
		hasID := false
		for _, t := range p.Sort {
			if t.Field == "id" {
				hasID = true
				break
			}
		}
		if !hasID {
			p.Sort = append(p.Sort, SortTerm{Field: "id"})
		}
	}
	p.Cursor = q.Get("cursor")
	return p, nil
}

type Cursor struct {
	TenantID string `json:"t"`
	Route    string `json:"r"`
	Filter   string `json:"f"`
	Sort     string `json:"s"`
	Offset   int    `json:"o"`
	LastID   string `json:"l,omitempty"`
}

type CursorSigner struct {
	key []byte
}

func NewCursorSigner(key []byte) (*CursorSigner, error) {
	if len(key) < 16 {
		return nil, errors.New("apicontract: cursor signing key must be at least 16 bytes")
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &CursorSigner{key: k}, nil
}

type cursorEnvelope struct {
	Version int    `json:"v"`
	Tenant  string `json:"t"`
	Route   string `json:"r"`
	Filter  string `json:"f"`
	Sort    string `json:"s"`
	Offset  int    `json:"o"`
	LastID  string `json:"l,omitempty"`
}

func (s *CursorSigner) Sign(c Cursor) (string, error) {
	if c.TenantID == "" || c.Route == "" {
		return "", errors.New("apicontract: a cursor needs a tenant and a route")
	}
	if c.Offset < 0 {
		return "", errors.New("apicontract: cursor offset cannot be negative")
	}
	raw, err := json.Marshal(cursorEnvelope{Version: 1, Tenant: c.TenantID, Route: c.Route,
		Filter: c.Filter, Sort: c.Sort, Offset: c.Offset, LastID: c.LastID})
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + s.mac(payload), nil
}

func (s *CursorSigner) Verify(token, tenantID, route, filter, sortKey string) (Cursor, error) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Cursor{}, ErrCursorInvalid
	}
	want := s.mac(parts[0])
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[1])) != 1 {
		return Cursor{}, ErrCursorInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Cursor{}, ErrCursorInvalid
	}
	var env cursorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != 1 {
		return Cursor{}, ErrCursorInvalid
	}
	if !sameScope(env.Tenant, tenantID) || !sameScope(env.Route, route) ||
		!sameScope(env.Filter, filter) || !sameScope(env.Sort, sortKey) {
		return Cursor{}, ErrCursorScope
	}
	return Cursor{TenantID: env.Tenant, Route: env.Route, Filter: env.Filter, Sort: env.Sort,
		Offset: env.Offset, LastID: env.LastID}, nil
}

func (s *CursorSigner) Next(p Page, tenantID, route string, offset int, lastID string) (string, error) {
	return s.Sign(Cursor{TenantID: tenantID, Route: route, Filter: p.FilterKey(), Sort: p.SortKey(),
		Offset: offset, LastID: lastID})
}

func (s *CursorSigner) mac(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func sameScope(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func ScopedRoute(path string) string {
	u, err := url.Parse(path)
	if err != nil {
		return path
	}
	return u.Path
}
