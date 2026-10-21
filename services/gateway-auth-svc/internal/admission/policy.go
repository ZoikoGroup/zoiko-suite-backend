package admission

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
)

// Policy is the request-shape and version admission policy.
//
// The gateway is a ForwardAuth target: it sees the original request's method,
// path and headers, never its body. So what it can enforce is what those say.
// Content-Length is checked against the cap; a body that arrives without a
// declared length (chunked) cannot be measured here and is bounded by the edge
// proxy's request-buffering limit instead.
type Policy struct {
	// MaxBodyBytes caps a declared Content-Length. 0 disables the check.
	MaxBodyBytes int64
	// AllowedEncodings are the Content-Encoding values a request body may carry.
	// Empty means no compressed request body is accepted: the gateway cannot
	// know how far a compressed body expands, so by default it refuses to
	// forward one (decompression bombs, INT-NP-004).
	AllowedEncodings []string
	// MaxCompressedBytes caps the declared length of an allowed compressed body.
	// 0 uses MaxBodyBytes.
	MaxCompressedBytes int64
	// SupportedVersions are the API versions ("v1") that may be routed. Empty
	// disables version admission.
	SupportedVersions []string
}

// Kind classifies a violation.
type Kind string

const (
	KindMalformed          Kind = "malformed_request"
	KindTooLarge           Kind = "request_too_large"
	KindEncodingRefused    Kind = "request_encoding_refused"
	KindVersionUnsupported Kind = "version_unsupported"
)

// Violation is a refusal with a status and a rule-naming detail. Detail never
// echoes caller input.
type Violation struct {
	Kind   Kind
	Status int
	Detail string
}

var versionSegment = regexp.MustCompile(`^v[0-9]+$`)

// Check applies the policy to the headers Traefik forwards on /verify. uri is
// the original request URI (X-Forwarded-Uri). It returns nil to admit.
//
// There is no fallback between versions: a request for an unsupported version
// is refused, never served by a different one (INT-NP-003).
func (p Policy) Check(h http.Header, uri string) (v *Violation, apiVersion string) {
	if cl := strings.TrimSpace(h.Get("Content-Length")); cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || n < 0 {
			return &Violation{KindMalformed, http.StatusBadRequest, "Content-Length is not a valid length"}, ""
		}
		if enc := contentEncoding(h); enc != "" {
			if !p.encodingAllowed(enc) {
				return &Violation{KindEncodingRefused, http.StatusUnsupportedMediaType,
					"compressed request bodies are not accepted"}, ""
			}
			limit := p.MaxCompressedBytes
			if limit <= 0 {
				limit = p.MaxBodyBytes
			}
			if limit > 0 && n > limit {
				return &Violation{KindTooLarge, http.StatusRequestEntityTooLarge,
					"compressed request body exceeds the admission limit"}, ""
			}
		} else if p.MaxBodyBytes > 0 && n > p.MaxBodyBytes {
			return &Violation{KindTooLarge, http.StatusRequestEntityTooLarge,
				"request body exceeds the admission limit"}, ""
		}
	} else if enc := contentEncoding(h); enc != "" && !p.encodingAllowed(enc) {
		return &Violation{KindEncodingRefused, http.StatusUnsupportedMediaType,
			"compressed request bodies are not accepted"}, ""
	}

	if len(p.SupportedVersions) == 0 {
		return nil, ""
	}
	pathVersion, ok := pathAPIVersion(uri)
	if !ok {
		return &Violation{KindMalformed, http.StatusBadRequest, "request URI is not valid"}, ""
	}
	headerVersion := strings.ToLower(strings.TrimSpace(firstNonEmpty(h.Get("X-API-Version"), h.Get("Accept-Version"))))
	if headerVersion != "" && !versionSegment.MatchString(headerVersion) {
		return &Violation{KindVersionUnsupported, http.StatusBadRequest, "API version header is not a valid version"}, ""
	}
	if pathVersion != "" && headerVersion != "" && pathVersion != headerVersion {
		return &Violation{KindVersionUnsupported, http.StatusBadRequest,
			"API version in the path conflicts with the version header"}, ""
	}
	version := firstNonEmpty(pathVersion, headerVersion)
	if version == "" {
		return nil, "" // an unversioned route: nothing to admit or refuse
	}
	if !p.versionSupported(version) {
		return &Violation{KindVersionUnsupported, http.StatusBadRequest,
			"API version is not supported"}, version
	}
	return nil, version
}

func (p Policy) versionSupported(v string) bool {
	for _, s := range p.SupportedVersions {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

func (p Policy) encodingAllowed(enc string) bool {
	for _, part := range strings.Split(enc, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" || part == "identity" {
			continue
		}
		ok := false
		for _, a := range p.AllowedEncodings {
			if strings.EqualFold(a, part) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// contentEncoding returns the declared encoding if it is anything but
// identity, or "" when the body is not (claimed to be) compressed.
func contentEncoding(h http.Header) string {
	enc := strings.ToLower(strings.TrimSpace(strings.Join(h.Values("Content-Encoding"), ",")))
	if enc == "" || enc == "identity" {
		return ""
	}
	return enc
}

// pathAPIVersion returns the "vN" first path segment, "" for an unversioned
// path, and ok=false for a URI that cannot be parsed.
func pathAPIVersion(uri string) (string, bool) {
	if uri == "" {
		return "", true
	}
	u, err := url.ParseRequestURI(uri)
	if err != nil {
		return "", false
	}
	seg := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)[0]
	seg = strings.ToLower(seg)
	if versionSegment.MatchString(seg) {
		return seg, true
	}
	return "", true
}

// RoutePath returns the path of uri without its query, for evidence and
// problem instances (queries can carry tokens and identifiers).
func RoutePath(uri string) string {
	if i := strings.IndexAny(uri, "?#"); i >= 0 {
		uri = uri[:i]
	}
	if len(uri) > 256 {
		uri = uri[:256]
	}
	return uri
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Gate sheds load above a concurrency ceiling (INT-BP-016). Admission fails
// fast rather than queueing: a verifier that queues under overload makes every
// gated request in the estate slow, and then fail, together.
type Gate struct {
	max int64
	n   atomic.Int64
}

// NewGate builds a gate. max <= 0 disables shedding.
func NewGate(max int) *Gate { return &Gate{max: int64(max)} }

// Acquire reserves a slot; false means the caller must refuse the request.
func (g *Gate) Acquire() bool {
	if g == nil || g.max <= 0 {
		return true
	}
	if g.n.Add(1) > g.max {
		g.n.Add(-1)
		return false
	}
	return true
}

// Release returns a slot taken by a successful Acquire.
func (g *Gate) Release() {
	if g == nil || g.max <= 0 {
		return
	}
	g.n.Add(-1)
}

// InFlight is the number of slots currently held.
func (g *Gate) InFlight() int64 {
	if g == nil {
		return 0
	}
	return g.n.Load()
}
