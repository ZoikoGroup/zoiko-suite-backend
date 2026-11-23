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

	"github.com/prometheus/client_golang/prometheus"
)

// IC8 holds all IC-8 instruments.
type IC8 struct {
	MFAVerifications *prometheus.CounterVec
	LoginLockouts    prometheus.Counter
	TOTPReplay       prometheus.Counter
	SessionsActive   prometheus.Gauge
	SessionRevoc     *prometheus.CounterVec
	PasswordResets   *prometheus.CounterVec
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
	}

	reg.MustRegister(
		m.MFAVerifications, m.LoginLockouts, m.TOTPReplay,
		m.SessionsActive, m.SessionRevoc, m.PasswordResets,
	)

	return m
}
