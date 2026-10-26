// Package idenv verifies identity-context-svc's signed identity envelope at a
// relying party, and carries it on to the next hop.
//
// It replaces the per-service copies (authorization-svc internal/identity,
// policy-svc internal/identity, gateway-auth-svc's /verify) that each did the
// signature check and nothing else. The audit of 14 Oct 2026 found what that
// left out:
//
//   - AU-01: a logged-out session kept working everywhere for the life of the
//     envelope (up to 300 s), because no relying party asked whether the
//     session still existed. Gate checks revocation through identity-context's
//     introspection endpoint with a short cache, and the first refusal of a
//     revoked session records how long revocation took to bite (IAM §26).
//   - AU-12: one audience for everything. Verifier pins the relying party's
//     own audience and accepts the legacy shared audience only while
//     AcceptLegacyAudience is set.
//   - AU-27: bearer envelopes replayed by anyone holding one. An envelope
//     that carries cnf is bound to a DPoP key (RFC 9449) or a client
//     certificate (RFC 8705); Gate refuses it without the matching proof.
//   - X-R3: every other service took X-Principal-Id / X-Tenant-Id on trust.
//     Gate makes the verified envelope the only source of those headers.
//   - AU-24: refusals are RFC 9457 problem+json, and RequestID adopts
//     Zoiko-Request-Id (ZS-API-001 §12) while still echoing X-Request-Id.
//
// Adopting it: go.mod `require zoiko.io/identity-envelope v0.0.0` and
// `replace zoiko.io/identity-envelope => ../identity-envelope`; build the
// service's image from services/ (see the service's Dockerfile). Then
//
//	gate, err := idenv.NewGateFromEnv("my-svc", log)
//	r.Use(idenv.RequestID, gate.Middleware)
//
// and, for outbound calls made on a person's behalf, idenv.Forward(ctx, req).
package idenv
