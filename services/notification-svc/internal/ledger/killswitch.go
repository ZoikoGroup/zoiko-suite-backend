package ledger

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// KillSwitchReason represents the operational rationale for an engaged kill switch.
type KillSwitchReason string

const (
	KillSwitchReasonSecurityIncident KillSwitchReason = "SECURITY_INCIDENT"
	KillSwitchReasonProviderOutage   KillSwitchReason = "PROVIDER_OUTAGE"
	KillSwitchReasonTenantLock       KillSwitchReason = "TENANT_LOCK"
	KillSwitchReasonTemplateDefect   KillSwitchReason = "TEMPLATE_DEFECT"
	KillSwitchReasonManualEmergency  KillSwitchReason = "MANUAL_EMERGENCY"
)

// KillSwitchEntry describes an active emergency kill-switch rule.
type KillSwitchEntry struct {
	Scope     string           `json:"scope"`  // "GLOBAL", "TENANT", "TEMPLATE"
	Target    string           `json:"target"` // tenant_id, template_key, or "*"
	Reason    KillSwitchReason `json:"reason"`
	EngagedBy string           `json:"engaged_by"`
	EngagedAt time.Time        `json:"engaged_at"`
}

// KillSwitchManager coordinates fail-closed emergency circuit breakers per §9.
type KillSwitchManager struct {
	mu        sync.RWMutex
	global    bool
	tenants   map[string]KillSwitchReason
	templates map[string]KillSwitchReason
	log       *zap.Logger
}

// NewKillSwitchManager creates an active in-memory kill switch manager.
func NewKillSwitchManager(log *zap.Logger) *KillSwitchManager {
	if log == nil {
		log = zap.NewNop()
	}
	return &KillSwitchManager{
		tenants:   make(map[string]KillSwitchReason),
		templates: make(map[string]KillSwitchReason),
		log:       log,
	}
}

// EngageGlobal activates a platform-wide emergency halt on email delivery.
func (k *KillSwitchManager) EngageGlobal(reason KillSwitchReason, actor string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.global = true
	k.log.Warn("EMERGENCY KILL SWITCH: global email delivery halted",
		zap.String("reason", string(reason)),
		zap.String("engaged_by", actor),
	)
}

// DisengageGlobal restores normal delivery if no tenant/template rule applies.
func (k *KillSwitchManager) DisengageGlobal(actor string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.global = false
	k.log.Info("EMERGENCY KILL SWITCH: global email delivery resumed", zap.String("disengaged_by", actor))
}

// EngageTenant blocks all email delivery for a specific tenant.
func (k *KillSwitchManager) EngageTenant(tenantID string, reason KillSwitchReason, actor string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.tenants[tenantID] = reason
	k.log.Warn("EMERGENCY KILL SWITCH: tenant delivery halted",
		zap.String("tenant_id", tenantID),
		zap.String("reason", string(reason)),
		zap.String("engaged_by", actor),
	)
}

// DisengageTenant clears the tenant-scoped kill switch.
func (k *KillSwitchManager) DisengageTenant(tenantID string, actor string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.tenants, tenantID)
	k.log.Info("EMERGENCY KILL SWITCH: tenant delivery resumed",
		zap.String("tenant_id", tenantID),
		zap.String("disengaged_by", actor),
	)
}

// EngageTemplate blocks delivery for a specific template key (e.g. "ZS-IA-001" or prefix "ZS-IA-*").
func (k *KillSwitchManager) EngageTemplate(templateKey string, reason KillSwitchReason, actor string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.templates[strings.ToUpper(strings.TrimSpace(templateKey))] = reason
	k.log.Warn("EMERGENCY KILL SWITCH: template delivery halted",
		zap.String("template_key", templateKey),
		zap.String("reason", string(reason)),
		zap.String("engaged_by", actor),
	)
}

// DisengageTemplate clears the template-scoped kill switch.
func (k *KillSwitchManager) DisengageTemplate(templateKey string, actor string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.templates, strings.ToUpper(strings.TrimSpace(templateKey)))
	k.log.Info("EMERGENCY KILL SWITCH: template delivery resumed",
		zap.String("template_key", templateKey),
		zap.String("disengaged_by", actor),
	)
}

// Check evaluates whether a message intent is blocked by any active kill switch.
// Returns (blocked, reason).
func (k *KillSwitchManager) Check(_ context.Context, tenantID, templateKey string) (bool, string) {
	k.mu.RLock()
	defer k.mu.RUnlock()

	// 1. Global kill switch
	if k.global {
		return true, string(KillSwitchReasonManualEmergency) + ": platform-wide delivery halted"
	}

	// 2. Tenant kill switch
	if r, ok := k.tenants[tenantID]; ok {
		return true, string(r) + ": tenant delivery halted"
	}

	// 3. Exact template kill switch
	normKey := strings.ToUpper(strings.TrimSpace(templateKey))
	if r, ok := k.templates[normKey]; ok {
		return true, string(r) + ": template delivery halted"
	}

	// 4. Wildcard prefix template kill switch (e.g. "ZS-IA-*")
	for tKey, r := range k.templates {
		if strings.HasSuffix(tKey, "*") {
			prefix := strings.TrimSuffix(tKey, "*")
			if strings.HasPrefix(normKey, prefix) {
				return true, string(r) + ": template family delivery halted"
			}
		}
	}

	return false, ""
}
