// Package metrics holds IC-8 observability instruments: MFA verifications,
// login lockouts, TOTP replay refusals, throttling, active sessions,
// revocations by reason, and password reset decisions. These are the
// golden-signal counters that the HTTP metrics alone cannot surface.
//
// Default is set from cmd/server/main.go so any package can increment
// without threading a metrics handle through every constructor.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"zoiko.io/identity-context-svc/internal/domain"
)

// IC8 holds all IC-8 instruments.
type IC8 struct {
	MFAVerifications *prometheus.CounterVec
	LoginLockouts    prometheus.Counter
	TOTPReplay       prometheus.Counter
	SessionsActive   prometheus.Gauge
	SessionRevoc     *prometheus.CounterVec
	PasswordResets   *prometheus.CounterVec
	// IAM-8: revocation SLO — latency from revocation to gateway /verify confirmation
	RevocationLag *prometheus.HistogramVec
}

var (
	defaultMu  sync.Mutex
	defaultIC8 *IC8
)

// Default returns the process-wide IC8, or nil if not yet initialised.
func Default() *IC8 {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	return defaultIC8
}

// SetDefault installs the process-wide IC8. Called once from main.go.
func SetDefault(m *IC8) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultIC8 = m
}

// New registers all IC-8 instruments with reg and returns them.
func New(reg prometheus.Registerer) *IC8 {
	m := &IC8{
		MFAVerifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "identity_mfa_verifications_total",
			Help: "Second-factor verifications by outcome (success or failure).",
		}, []string{"outcome"}),
		LoginLockouts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "identity_login_lockouts_total",
			Help: "Credential lockouts triggered after threshold failures.",
		}),
		TOTPReplay: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "identity_totp_replay_refusals_total",
			Help: "TOTP codes refused as replayed or already-used.",
		}),
		SessionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "identity_sessions_active",
			Help: "Live sessions held by active principals, gauge.",
		}),
		SessionRevoc: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "identity_session_revocations_total",
			Help: "Sessions revoked by reason.",
		}, []string{"reason"}),
		PasswordResets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "identity_password_resets_total",
			Help: "Password reset requests by decision (approved, denied, expired).",
		}, []string{"decision"}),
		RevocationLag: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "identity_revocation_lag_seconds",
			Help:    "Latency from session revocation to gateway /verify confirmation (IAM-8).",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}, []string{"reason"}),
	}

	reg.MustRegister(
		m.MFAVerifications, m.LoginLockouts, m.TOTPReplay,
		m.SessionsActive, m.SessionRevoc, m.PasswordResets,
		m.RevocationLag,
	)

	return m
}

// ObserveRevocationLag records the latency from revocation to gateway confirmation.
// Called by the gateway /verify endpoint when it confirms a session is revoked.
// revokedAt is the time the revocation was issued (from the session_contexts.invalidated_at).
// reason is the InvalidationReason for labeling.
func (m *IC8) ObserveRevocationLag(revokedAt time.Time, reason domain.InvalidationReason) {
	if m == nil || m.RevocationLag == nil {
		return
	}
	lag := time.Since(revokedAt).Seconds()
	m.RevocationLag.WithLabelValues(string(reason)).Observe(lag)
}
