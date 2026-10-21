package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	svcenvelope "zoiko.io/vendor-due-diligence-svc/internal/envelope"
)

const decisionCacheTTL = 5 * time.Second

type cachedDecision struct {
	deniedErr error
	expiresAt time.Time
}

type Client struct {
	baseURL string
	client  *http.Client
	log     *zap.Logger

	cacheMu     sync.Mutex
	cache       map[string]cachedDecision
	cacheWrites int
}

func NewClient(baseURL string, client *http.Client, log *zap.Logger) *Client {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  client,
		log:     log,
		cache:   make(map[string]cachedDecision),
	}
}

func (a *Client) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	key := principalID + "|" + legalEntityID + "|" + actionType

	if decision, hit := a.lookupCache(key); hit {
		return decision
	}

	err := a.checkAllowedLive(ctx, principalID, legalEntityID, actionType)

	if err == nil || errors.Is(err, domain.ErrAuthorizationDenied) {
		a.storeCache(key, err)
	}

	return err
}

func (a *Client) lookupCache(key string) (error, bool) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()

	d, ok := a.cache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(d.expiresAt) {
		delete(a.cache, key)
		return nil, false
	}
	return d.deniedErr, true
}

func (a *Client) storeCache(key string, decision error) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()

	a.cache[key] = cachedDecision{deniedErr: decision, expiresAt: time.Now().Add(decisionCacheTTL)}

	a.cacheWrites++
	if a.cacheWrites%1000 == 0 {
		now := time.Now()
		for k, v := range a.cache {
			if now.After(v.expiresAt) {
				delete(a.cache, k)
			}
		}
	}
}

func (a *Client) checkAllowedLive(ctx context.Context, principalID, legalEntityID, actionType string) error {
	reqBody, _ := json.Marshal(map[string]string{
		"principal_id":    principalID,
		"legal_entity_id": legalEntityID,
		"action_type":     actionType,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/authorize", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", principalID)
	req.Header.Set("X-Legal-Entity-Id", legalEntityID)

	authzRequestID := middleware.GetReqID(ctx)
	authzSourceChannel := "system"
	if env, ok := svcenvelope.FromContext(ctx); ok {
		if env.TenantID != "" {
			req.Header.Set("X-Tenant-Id", env.TenantID)
		}
		if env.RequestID != "" {
			authzRequestID = env.RequestID
		}
		if env.SourceChannel != "" {
			authzSourceChannel = string(env.SourceChannel)
		}
		if env.CorrelationID != "" {
			req.Header.Set("X-Correlation-ID", env.CorrelationID)
		}
		if env.CausationID != "" {
			req.Header.Set("X-Causation-Id", env.CausationID)
		}
	}
	req.Header.Set("X-Request-Id", authzRequestID)
	req.Header.Set("X-Source-Channel", authzSourceChannel)
	req.Header.Set("Idempotency-Key", authzRequestID+":"+actionType)
	resp, err := a.client.Do(req)
	if err != nil {
		a.log.Error("failed to call authorization-svc", zap.Error(err))
		return domain.ErrAuthzServiceUnavailabe
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return domain.ErrAuthzServiceUnavailabe
	}

	var res struct {
		DecisionOutcome string `json:"decision_outcome"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return err
	}
	if res.DecisionOutcome != "GRANTED" {
		return domain.ErrAuthorizationDenied
	}
	return nil
}
