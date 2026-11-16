// Package catalogref is the AR client for the BIZ-07 Product & Service Catalog
// (hosted by inventory-management-svc).
//
// AR's catalog_version_ref has always been an opaque string. A ref of the form
//
//	catalog:<offering_id>:<version_id>
//
// (both UUIDs) opts a quote or order into governed pinning: before the protected
// step (approval) AR asks the catalog whether that exact version is usable, i.e.
// ACTIVE and carrying every required mapping. Any other ref stays opaque and is
// never sent to the catalog, so existing documents are unaffected.
//
// "Cannot answer" is never "usable": a transport failure or an unexpected status
// is ErrUnavailable and callers fail closed.
package catalogref

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/svcclient"
)

const prefix = "catalog:"

var refRe = regexp.MustCompile(`^catalog:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}):([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

// ErrUnavailable means the catalog could not give an answer.
var ErrUnavailable = errors.New("catalog unavailable")

// ErrMalformed means a ref claims to be a catalog ref but is not well-formed.
var ErrMalformed = errors.New("catalog_version_ref starting with 'catalog:' must be catalog:<offering_id>:<version_id> with both ids UUIDs")

// IsCatalogRef reports whether ref opts into governed catalog pinning.
func IsCatalogRef(ref string) bool { return strings.HasPrefix(ref, prefix) }

// Parse splits a catalog ref. ok is false for an opaque (non-catalog) ref;
// err is ErrMalformed for a catalog-prefixed ref that does not parse.
func Parse(ref string) (offeringID, versionID string, ok bool, err error) {
	if !IsCatalogRef(ref) {
		return "", "", false, nil
	}
	m := refRe.FindStringSubmatch(ref)
	if m == nil {
		return "", "", false, ErrMalformed
	}
	return strings.ToLower(m[1]), strings.ToLower(m[2]), true, nil
}

// Result is the catalog's verdict on one version.
type Result struct {
	Usable          bool     `json:"usable"`
	Status          string   `json:"status"`
	MissingMappings []string `json:"missing_mappings"`
	Reason          string   `json:"reason"`
	// Found is false when the catalog has no such version for the tenant.
	Found bool `json:"-"`
}

// Checker asks the catalog whether a version is usable.
type Checker interface {
	Check(ctx context.Context, caller svcclient.Caller, offeringID, versionID string) (*Result, error)
}

// HTTPChecker calls GET /v1/catalog/offerings/{id}/versions/{versionID}/usability.
type HTTPChecker struct{ c *svcclient.Client }

func NewHTTPChecker(baseURL string) *HTTPChecker { return &HTTPChecker{c: svcclient.New(baseURL)} }

func NewHTTPCheckerWith(c *svcclient.Client) *HTTPChecker { return &HTTPChecker{c: c} }

func (h *HTTPChecker) Check(ctx context.Context, caller svcclient.Caller, offeringID, versionID string) (*Result, error) {
	path := "/v1/catalog/offerings/" + url.PathEscape(offeringID) + "/versions/" + url.PathEscape(versionID) + "/usability"
	status, body, err := h.c.Do(ctx, caller, http.MethodGet, path, nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	switch status {
	case http.StatusOK:
		var r Result
		if json.Unmarshal(body, &r) != nil {
			return nil, ErrUnavailable
		}
		r.Found = true
		return &r, nil
	case http.StatusNotFound:
		return &Result{Found: false}, nil
	default:
		// 401/403/5xx and anything unexpected: no answer, not "no problem".
		return nil, ErrUnavailable
	}
}

// Require enforces governed pinning for one document ref at a protected step.
//
//   - an opaque ref (or none) -> nil: unchanged behaviour;
//   - checker == nil with a catalog ref -> 503: AR cannot verify it, fail closed;
//   - catalog unavailable -> 503;
//   - version unknown or not usable -> 422 CATALOG_VERSION_INVALID, the spec's own
//     stable error ("Offering version missing, inactive or incompatible with
//     requested use").
func Require(ctx context.Context, checker Checker, caller svcclient.Caller, ref string) error {
	offeringID, versionID, ok, err := Parse(ref)
	if err != nil {
		return kit.Invalid(err.Error())
	}
	if !ok {
		return nil
	}
	if checker == nil {
		return kit.Unavailable("catalog service not wired: the pinned catalog version cannot be verified")
	}
	res, err := checker.Check(ctx, caller, offeringID, versionID)
	switch {
	case err != nil:
		return kit.Unavailable("catalog service unavailable: the pinned catalog version cannot be verified")
	case !res.Found:
		return kit.Unprocessable(kit.CodeCatalogVersionInvalid, "the pinned catalog version does not exist")
	case !res.Usable:
		msg := "the pinned catalog version is not usable"
		if res.Reason != "" {
			msg += ": " + res.Reason
		}
		return kit.Unprocessable(kit.CodeCatalogVersionInvalid, msg)
	}
	return nil
}

// SystemPrincipal is the identity AR uses when asking the catalog. The check is
// a service-to-service read, not an act of the approving user, so the catalog
// grant (CATALOG_READ) is held by this principal rather than by every approver.
const SystemPrincipal = "system:ar-catalog"

// AsSystem returns caller with the AR catalog system principal.
func AsSystem(caller svcclient.Caller) svcclient.Caller {
	caller.PrincipalID = SystemPrincipal
	return caller
}
