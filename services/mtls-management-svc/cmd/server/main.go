package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/mtls-management-svc/internal/authz"
	"zoiko.io/mtls-management-svc/internal/ca"
	"zoiko.io/mtls-management-svc/internal/handler"
	"zoiko.io/mtls-management-svc/internal/siem"
	"zoiko.io/mtls-management-svc/internal/store"
)

// serviceNamePattern is what a bootstrap token directory may be named.
var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// loadBootstrapTokens reads the per-service self-provisioning tokens from
// MTLS_BOOTSTRAP_TOKENS_DIR: one sub-directory per service, named for it,
// holding a file "token" (deployments/docker-compose.yml's
// mtls-bootstrap-keygen writes exactly that). Each service mounts only its
// own sub-directory, so it can read only its own token (register M-01).
//
// Tokens live in files rather than env vars so they never appear in
// `docker inspect` or a process listing. An unset directory disables the
// bootstrap path: this service still starts and serves the principal +
// authorize path. A malformed directory — a token shared by two services, an
// unreadable or misnamed entry — refuses to start, because it would bind a
// token to the wrong identity or to more than one.
func loadBootstrapTokens(logger *zap.Logger) (handler.BootstrapTokens, error) {
	if os.Getenv("MTLS_BOOTSTRAP_TOKEN_PATH") != "" {
		return nil, fmt.Errorf("MTLS_BOOTSTRAP_TOKEN_PATH (one token shared by every service) is no longer accepted: " +
			"any holder could mint a certificate for any service (Governance register M-01). " +
			"Set MTLS_BOOTSTRAP_TOKENS_DIR to a directory of <service-name>/token files")
	}
	dir := os.Getenv("MTLS_BOOTSTRAP_TOKENS_DIR")
	if dir == "" {
		logger.Info("MTLS_BOOTSTRAP_TOKENS_DIR not set — self-provisioning bootstrap path disabled")
		return nil, nil
	}
	return readBootstrapTokens(dir, logger)
}

func readBootstrapTokens(dir string, logger *zap.Logger) (handler.BootstrapTokens, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read MTLS_BOOTSTRAP_TOKENS_DIR %q: %w", dir, err)
	}
	tokens := handler.BootstrapTokens{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		service := e.Name()
		if !serviceNamePattern.MatchString(service) {
			return nil, fmt.Errorf("bootstrap token directory %q is not a service name", service)
		}
		data, err := os.ReadFile(filepath.Join(dir, service, "token"))
		if err != nil {
			return nil, fmt.Errorf("read bootstrap token for %s: %w", service, err)
		}
		token := strings.TrimSpace(string(data))
		if len(token) < 32 {
			return nil, fmt.Errorf("bootstrap token for %s is shorter than 32 characters", service)
		}
		if other, dup := tokens[token]; dup {
			return nil, fmt.Errorf("services %s and %s share a bootstrap token", other, service)
		}
		tokens[token] = service
	}
	services := make([]string, 0, len(tokens))
	for _, s := range tokens {
		services = append(services, s)
	}
	logger.Info("bootstrap tokens loaded", zap.Strings("services", services))
	return tokens, nil
}

// newAuthzClient talks to authorization-svc. With AUTHZ_MTLS_URL set it calls
// the mTLS listener with this service's own self-issued client certificate,
// which authorization-svc's workload gate requires outside ENV=local;
// otherwise the plaintext AUTHZ_SERVICE_URL.
func newAuthzClient(logger *zap.Logger, internalCA *ca.CA, ledger *ca.Ledger) *authz.Client {
	if mtlsURL := os.Getenv("AUTHZ_MTLS_URL"); mtlsURL != "" {
		identity := ca.NewClientIdentity(internalCA, ledger, "mtls-management-svc")
		if _, err := identity.Certificate(); err != nil {
			logger.Fatal("failed to issue this service's own mTLS client certificate", zap.Error(err))
		}
		logger.Info("authorization-svc calls use mTLS", zap.String("url", mtlsURL))
		return authz.NewClientWithHTTPClient(mtlsURL, &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: identity.TLSConfig()},
		})
	}
	// Fail fast rather than starting and 503-ing every guarded route.
	// authz.NewClient("") builds requests against an empty base URL, so every
	// CheckAllowed fails and the client — correctly — refuses. That posture is
	// right at request time but wrong at boot: a service that starts and then
	// rejects all authorized traffic hides a deployment mistake until a user
	// finds it. This dependency was added in Priority 2b and was missing from
	// docker-compose.yml for two services, which is exactly how that happens.
	authzURL := os.Getenv("AUTHZ_SERVICE_URL")
	if authzURL == "" {
		logger.Fatal("AUTHZ_SERVICE_URL (or AUTHZ_MTLS_URL) is required: every authorized route would return 503 without it")
	}
	if env := os.Getenv("ENV"); env != "" && !strings.EqualFold(env, "local") {
		logger.Warn("authorization-svc called over plaintext outside ENV=local — its evaluation gate refuses that in require mode; set AUTHZ_MTLS_URL")
	}
	return authz.NewClient(authzURL)
}

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8140"
	}

	caDir := os.Getenv("CA_DATA_DIR")
	if caDir == "" {
		caDir = "/data/ca"
	}
	internalCA, err := ca.LoadOrCreate(caDir)
	if err != nil {
		logger.Fatal("failed to load or create internal CA", zap.String("ca_data_dir", caDir), zap.Error(err))
	}
	logger.Info("internal CA ready", zap.String("ca_data_dir", caDir))
	// The revocation ledger sits beside the CA key, on the same volume: a
	// revocation must outlive a restart exactly as long as the CA does.
	ledger, err := ca.OpenLedger(caDir)
	if err != nil {
		logger.Fatal("failed to open revocation ledger", zap.String("ca_data_dir", caDir), zap.Error(err))
	}

	s := store.NewMemoryStore()
	siemClient := siem.New(os.Getenv("SIEM_SERVICE_URL"), "mtls-management-svc", logger)

	// Drain accepted SIEM events on shutdown. Stream returns before delivery,
	// so without this a SIGTERM would discard security events already accepted.
	defer siemClient.Close()
	authzClient := newAuthzClient(logger, internalCA, ledger)
	bootstrapTokens, err := loadBootstrapTokens(logger)
	if err != nil {
		logger.Fatal("bootstrap tokens", zap.Error(err))
	}
	h := handler.New(s, internalCA, ledger, siemClient, authzClient, logger, bootstrapTokens)

	// Create router with apicontract middleware
	r := chi.NewRouter()
	r.Use(apicontract.RequestID)
	r.Use(apicontract.DefaultCache)
	// Mount the handler's routes
	r.Mount("/", handler.NewRouter(h))

	srv := &http.Server{Addr: ":" + port, Handler: r, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()
	logger.Info("mtls-management-svc listening on :" + port)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
