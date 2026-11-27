// Package certreliance makes general-ledger-svc a recorded consumer of certified evidence (ZS-SVC-Z-001 RB-05, DQC-017).
//
// A journal whose evidence cites a data-quality or reconciliation certification rests on it. When that certification is
// later revoked, the owner of the journal must be able to find out, so the journal RECORDS its reliance with the service that
// issued the certification (POST .../certification/reliances), and the revocation's blast radius then names it.
//
// A journal cites a certification with an evidence reference of the form
//
//	certification:<service>:<run_id>        service is data-quality-svc or reconciliation-engine-svc
//
// Any other evidence reference is none of this package's business and is ignored. A reference that starts with
// "certification:" but is malformed or names an unknown service is REFUSED, never skipped: a typo must not silently turn a
// journal that rests on certified evidence into one that nobody will ever be told about.
package certreliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// RefPrefix starts every evidence reference that cites a certification.
	RefPrefix = "certification:"

	ServiceDataQuality    = "data-quality-svc"
	ServiceReconciliation = "reconciliation-engine-svc"

	// RelyingService and ObjectTypeJournal are what the journal is recorded as.
	RelyingService    = "general-ledger-svc"
	ObjectTypeJournal = "journal"
	// ObjectTypeAccountingPosting is a kernel posting: one accounting event posted in one book on one basis. It is the relying
	// object of the kernel path because its identity is stable across retries, while the journal id is new on every attempt.
	ObjectTypeAccountingPosting = "accounting-posting"

	maxRefsPerJournal = 20
)

var (
	// ErrInvalidRef: a "certification:" reference that is malformed or names an unknown service.
	ErrInvalidRef = errors.New("invalid certification evidence reference")
	// ErrNotInForce: the certification is revoked, superseded or past its validity window (the issuing service answered 409).
	ErrNotInForce = errors.New("the certification is not in force")
	// ErrUnknown: the issuing service has no such certification (404).
	ErrUnknown = errors.New("no such certification")
	// ErrDenied: the principal may not record reliance (401/403).
	ErrDenied = errors.New("not permitted to record reliance on this certification")
	// ErrUnavailable: the issuing service could not be reached or answered something unexpected, or is not configured.
	ErrUnavailable = errors.New("the certification service is unavailable")
)

var runIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Ref is one certification a journal cites.
type Ref struct {
	Service string
	RunID   string
}

func (r Ref) String() string { return RefPrefix + r.Service + ":" + r.RunID }

// ParseRefs picks the certification references out of a journal's evidence references, in order and without duplicates.
// Evidence that does not start with RefPrefix is ignored. A malformed one is an error naming it.
func ParseRefs(evidenceRefs []string) ([]Ref, error) {
	var out []Ref
	seen := map[Ref]bool{}
	for _, raw := range evidenceRefs {
		raw = strings.TrimSpace(raw)
		if !strings.HasPrefix(raw, RefPrefix) {
			continue
		}
		service, runID, ok := strings.Cut(strings.TrimPrefix(raw, RefPrefix), ":")
		if !ok || (service != ServiceDataQuality && service != ServiceReconciliation) || !runIDRe.MatchString(runID) {
			return nil, fmt.Errorf("%w: %q (want %s<%s|%s>:<run id>)", ErrInvalidRef, raw, RefPrefix, ServiceDataQuality, ServiceReconciliation)
		}
		ref := Ref{Service: service, RunID: runID}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
		if len(out) > maxRefsPerJournal {
			return nil, fmt.Errorf("%w: at most %d certification references per journal", ErrInvalidRef, maxRefsPerJournal)
		}
	}
	return out, nil
}

// Object is the relying object being recorded. Type is ObjectTypeJournal when empty.
type Object struct {
	Type          string
	ID            string
	LegalEntityID string
}

// Client records reliance with the services that issue certifications. Base URLs come only from server configuration; a
// journal can name a service from a closed set but can never supply a URL (no SSRF surface).
type Client struct {
	urls map[string]string
	http *http.Client
}

// NewClient takes the base URLs of data-quality-svc and reconciliation-engine-svc. A service with no URL is not configured:
// recording against it fails closed with ErrUnavailable.
func NewClient(dataQualityURL, reconciliationURL string) *Client {
	trim := func(s string) string { return strings.TrimRight(strings.TrimSpace(s), "/") }
	return &Client{
		urls: map[string]string{ServiceDataQuality: trim(dataQualityURL), ServiceReconciliation: trim(reconciliationURL)},
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

type relianceBody struct {
	RelyingService    string `json:"relying_service"`
	RelyingObjectType string `json:"relying_object_type"`
	RelyingObjectID   string `json:"relying_object_id"`
	Purpose           string `json:"purpose"`
}

// RecordReliance records that obj relied on ref's certification. The issuing service is idempotent by the reliance's natural
// key, so a retry (or a journal posted again after a transient failure) records nothing new.
func (c *Client) RecordReliance(ctx context.Context, tenantID, principalID, correlationID string, ref Ref, obj Object) error {
	base := c.urls[ref.Service]
	if base == "" {
		return fmt.Errorf("%w: %s is not configured", ErrUnavailable, ref.Service)
	}
	if !runIDRe.MatchString(ref.RunID) {
		return fmt.Errorf("%w: %q", ErrInvalidRef, ref.RunID)
	}
	var path string
	switch ref.Service {
	case ServiceDataQuality:
		path = "/v1/data-quality/runs/" + url.PathEscape(ref.RunID) + "/certification/reliances"
	case ServiceReconciliation:
		path = "/v1/reconciliation/runs/" + url.PathEscape(ref.RunID) + "/certification/reliances"
	default:
		return fmt.Errorf("%w: %q", ErrInvalidRef, ref.Service)
	}
	objType, purpose := obj.Type, "journal posted with this certification as its evidence"
	switch objType {
	case "", ObjectTypeJournal:
		objType = ObjectTypeJournal
	case ObjectTypeAccountingPosting:
		purpose = "accounting event posted with this certification as its evidence"
	default:
		return fmt.Errorf("%w: relying object type %q", ErrInvalidRef, objType)
	}
	body, err := json.Marshal(relianceBody{RelyingService: RelyingService, RelyingObjectType: objType, RelyingObjectID: obj.ID, Purpose: purpose})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", obj.LegalEntityID)
	req.Header.Set("X-Correlation-ID", correlationID)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, ref.Service, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrNotInForce, ref)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrUnknown, ref)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrDenied, ref)
	default:
		return fmt.Errorf("%w: %s answered HTTP %d", ErrUnavailable, ref.Service, resp.StatusCode)
	}
}
