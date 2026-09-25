// Package inboundmtls supplies the inbound transport security for
// secret-vault-integration-svc (compliance-close "no inbound mTLS").
//
// Three cooperating pieces:
//   - NewServerTLSConfig builds the *tls.Config for serving HTTPS.
//   - MutualTLS is wired through that config: when TLS_CLIENT_CA_FILE is
//     set, ClientAuth = RequireAndVerifyClientCert makes the handshake
//     itself refuse any client without a CA-signed certificate.
//   - IdentityCheck (a chi middleware) enforces the second line: the
//     presented certificate's identity must match the X-Workload-Id /
//     X-Principal-Id header the gateway verified, so a certificate issued
//     to one workload cannot be silently presented as another.
//
// Container/gateway TLS (the CA, cert provisioning, the gateway->svc hop)
// is the CROSS-SERVICE/infra requirement; this package makes the
// in-service half real.
package inboundmtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.uber.org/zap"
)

// NewServerTLSConfig returns the *tls.Config for serving inbound HTTPS.
// clientCAFile is optional; when empty, the returned config does NOT
// request client certificates (plain TLS, not mTLS).
func NewServerTLSConfig(certFile, keyFile, clientCAFile string, log *zap.Logger) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}

	conf := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}

	if clientCAFile != "" {
		pem, err := os.ReadFile(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read client CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("client CA file %q contains no usable certificates", clientCAFile)
		}
		conf.ClientCAs = pool
		conf.ClientAuth = tls.RequireAndVerifyClientCert
	}

	if log != nil {
		log.Info("inbound TLS configured",
			zap.String("cert_file", certFile),
			zap.String("client_ca_file", clientCAFile),
			zap.Bool("mutual", clientCAFile != ""),
		)
	}
	return conf, nil
}

// IdentityCheck returns a chi middleware that, when the presented client
// certificate (if any) does not name the identity claimed in the
// X-Workload-Id / X-Principal-Id headers, refuses the request. When
// checkIdentity is false it is a transparent no-op — the deployment wiring
// decides whether identity revalidation is on, not this function.
//
// If the client presented no certificate at all (plain-TLS deployment, or
// an mTLS terminating proxy that swallowed it), the middleware lets the
// request through: the decision about whether that is acceptable belongs
// to the config gate that decided whether to run mTLS, not to a
// best-effort middleware.
func IdentityCheck(checkIdentity bool, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !checkIdentity {
				next.ServeHTTP(w, r)
				return
			}
			if !rejectsIdentity(w, r, log) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

func rejectsIdentity(w http.ResponseWriter, r *http.Request, log *zap.Logger) bool {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		// No client certificate is visible (plain TLS, or a proxy). The
		// verified identity headers alone remain authority here — without a
		// cert there is nothing to cross-check the header against.
		return false
	}

	claimed := strings.TrimSpace(r.Header.Get("X-Workload-Id"))
	if claimed == "" {
		claimed = strings.TrimSpace(r.Header.Get("X-Principal-Id"))
	}
	if claimed == "" {
		if log != nil {
			log.Warn("inbound mTLS identity check: no verified identity header present to match cert against",
				zap.String("remote_addr", r.RemoteAddr))
		}
		http.Error(w, `{"error":"identity_claim_missing"}`, http.StatusUnauthorized)
		return true
	}

	leaf := r.TLS.PeerCertificates[0]
	if !identityMatches(claimed, leaf) {
		if log != nil {
			log.Warn("inbound mTLS identity check refused: cert identity differs from verified header",
				zap.String("claimed", claimed),
				zap.String("cert_cn", leaf.Subject.CommonName),
				zap.String("remote_addr", r.RemoteAddr))
		}
		http.Error(w, `{"error":"identity_mismatch"}`, http.StatusForbidden)
		return true
	}
	return false
}

// identityMatches reports whether the presented certificate's identity
// names the same workload/principal that the gateway header claims.
// Matching is deliberately simple: the certificate's Subject CN or any of
// its URI/dNS SANs must equal the claimed identity exactly.
func identityMatches(claimed string, leaf *x509.Certificate) bool {
	if leaf == nil || claimed == "" {
		return false
	}
	if leaf.Subject.CommonName == claimed {
		return true
	}
	for _, uri := range leaf.URIs {
		u := uri.String()
		if u == claimed {
			return true
		}
		// Allow the x509 "spiffe-ish" shape spiffe://<domain>/identity/<id>
		// to carry the identity as the last path segment.
		if idx := strings.LastIndex(u, "/"); idx >= 0 && u[idx+1:] == claimed {
			return true
		}
	}
	for _, d := range leaf.DNSNames {
		if d == claimed {
			return true
		}
	}
	return false
}