// Package apicontract implements the shared wire contract every ZoikoSuite
// service must follow: RFC 9457 problem details with ZS-API-001 §12's
// extensions, the request-id and cache-control defaults, ETag/If-Match
// concurrency, signed opaque cursors, Location and Deprecation/Sunset helpers,
// and the asynchronous-operation model.
//
// It lives in the identity-envelope module because every service already
// imports that module through a replace directive.
package apicontract
