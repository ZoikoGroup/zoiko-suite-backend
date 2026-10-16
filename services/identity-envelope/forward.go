package idenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Forward copies the caller's verified identity onto an outbound request made
// on that person's behalf (X-82i, X-3): the envelope in Authorization, the
// identity headers it vouches for, and the authentication age the next hop's
// step-up checks read. A peer-basis or asserted request forwards only the
// headers it carried. It never overwrites an Authorization the caller set.
func Forward(ctx context.Context, req *http.Request) {
	id, ok := FromContext(ctx)
	if !ok {
		return
	}
	if id.Token != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+id.Token)
	}
	stampOutbound(req, id)
}

// ForwardTo is Forward with the envelope exchanged for audience first
// (AU-12), so the next hop receives an envelope only it accepts. With no
// exchanger, or on an exchange failure, it falls back to Forward and returns
// the error so the caller can log it.
func ForwardTo(ctx context.Context, req *http.Request, ex *Exchanger, audience string) error {
	id, ok := FromContext(ctx)
	if !ok {
		return nil
	}
	if ex == nil || id.Token == "" || audience == "" || req.Header.Get("Authorization") != "" {
		Forward(ctx, req)
		return nil
	}
	tok, err := ex.Exchange(ctx, id.Token, audience)
	if err != nil {
		Forward(ctx, req)
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	stampOutbound(req, id)
	return nil
}

func stampOutbound(req *http.Request, id *Identity) {
	if id.PrincipalID != "" && req.Header.Get(HeaderPrincipalID) == "" {
		req.Header.Set(HeaderPrincipalID, id.PrincipalID)
	}
	if id.TenantID != "" && req.Header.Get(HeaderTenantID) == "" {
		req.Header.Set(HeaderTenantID, id.TenantID)
	}
	if id.Claims != nil {
		if sc := id.Claims.SessionContextID(); sc != "" {
			req.Header.Set(HeaderSessionContextID, sc)
		}
		if age, ok := id.AuthnAge(time.Now()); ok {
			req.Header.Set(HeaderAuthnAge, strconv.FormatInt(int64(age/time.Second), 10))
		}
	}
}

// AuthnAgeHeader returns the X-Authn-Age-Seconds value to forward for ctx's
// caller, "" when unknown (clients that build headers by hand).
func AuthnAgeHeader(ctx context.Context) string {
	id, ok := FromContext(ctx)
	if !ok {
		return ""
	}
	age, ok := id.AuthnAge(time.Now())
	if !ok {
		return ""
	}
	return strconv.FormatInt(int64(age/time.Second), 10)
}

// TokenFrom returns ctx's raw envelope, "" when the request had none.
func TokenFrom(ctx context.Context) string {
	if id, ok := FromContext(ctx); ok {
		return id.Token
	}
	return ""
}

// Exchanger trades an envelope for one with a narrower audience at
// identity-context-svc's token endpoint (RFC 8693 token exchange). The
// exchanged envelope keeps the session, so revoking it revokes both, and names
// the exchanging workload in act. Results are cached until shortly before
// they expire.
type Exchanger struct {
	tokenURL string
	client   *http.Client
	// ClientID / ClientSecret authenticate this workload at the token
	// endpoint when it is not calling over mTLS.
	clientID, clientSecret string

	mu    sync.Mutex
	cache map[string]exchanged
}

type exchanged struct {
	token string
	until time.Time
}

// NewExchanger builds one. client should be the workload's mTLS client in
// production; nil uses a plain 5 s client.
func NewExchanger(tokenURL, clientID, clientSecret string, client *http.Client) *Exchanger {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &Exchanger{tokenURL: tokenURL, client: client, clientID: clientID, clientSecret: clientSecret,
		cache: map[string]exchanged{}}
}

// Exchange returns an envelope for audience.
func (e *Exchanger) Exchange(ctx context.Context, subjectToken, audience string) (string, error) {
	sum := sha256.Sum256([]byte(subjectToken))
	key := hex.EncodeToString(sum[:]) + "|" + audience
	now := time.Now()
	e.mu.Lock()
	if c, ok := e.cache[key]; ok && now.Before(c.until) {
		e.mu.Unlock()
		return c.token, nil
	}
	e.mu.Unlock()

	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {subjectToken},
		"subject_token_type":   {"urn:zoikosuite:params:oauth:token-type:identity-envelope"},
		"requested_token_type": {"urn:zoikosuite:params:oauth:token-type:identity-envelope"},
		"audience":             {audience},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if e.clientID != "" {
		req.SetBasicAuth(url.QueryEscape(e.clientID), url.QueryEscape(e.clientSecret))
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("token exchange: status %d %s %s", resp.StatusCode, out.Error, out.Description)
	}
	if out.ExpiresIn <= 0 {
		return "", errors.New("token exchange: no expires_in")
	}
	ttl := time.Duration(out.ExpiresIn)*time.Second - 30*time.Second
	if ttl > 0 {
		e.mu.Lock()
		if len(e.cache) > 4096 {
			for k, c := range e.cache {
				if now.After(c.until) {
					delete(e.cache, k)
				}
			}
		}
		e.cache[key] = exchanged{token: out.AccessToken, until: now.Add(ttl)}
		e.mu.Unlock()
	}
	return out.AccessToken, nil
}
