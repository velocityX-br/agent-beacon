package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OIDCConfig configures the OpenID Connect authorization-code provider.
type OIDCConfig struct {
	Issuer       string   // e.g. https://accounts.example.com
	ClientID     string
	ClientSecret string
	RedirectURL  string   // e.g. https://dashboard/api/v1/auth/callback
	Scopes       []string // defaults to ["openid","email","profile"]
}

// oidcDiscovery is the subset of the provider metadata document we use.
type oidcDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// OIDCProvider performs the authorization-code flow against a discovered issuer.
type OIDCProvider struct {
	cfg    OIDCConfig
	disco  oidcDiscovery
	client *http.Client

	mu     sync.Mutex
	states map[string]time.Time // CSRF state -> issued-at
}

// NewOIDCProvider discovers the issuer's endpoints and returns a provider.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (*OIDCProvider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc: issuer, client_id, and redirect_url are required")
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "email", "profile"}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	discoURL := strings.TrimRight(cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery: status %d", resp.StatusCode)
	}
	var d oidcDiscovery
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("oidc discovery decode: %w", err)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return nil, errors.New("oidc discovery: missing endpoints")
	}
	return &OIDCProvider{cfg: cfg, disco: d, client: client, states: map[string]time.Time{}}, nil
}

// AuthCodeURL creates a fresh CSRF state and returns the URL to redirect the
// browser to for login.
func (p *OIDCProvider) AuthCodeURL() (string, error) {
	state, err := randomState()
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.gcStatesLocked()
	p.states[state] = time.Now()
	p.mu.Unlock()

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", p.cfg.RedirectURL)
	q.Set("scope", strings.Join(p.cfg.Scopes, " "))
	q.Set("state", state)
	return p.disco.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// Exchange validates the returned state, swaps the code for tokens, and returns
// the authenticated subject (email/sub from userinfo).
func (p *OIDCProvider) Exchange(ctx context.Context, code, state string) (string, error) {
	p.mu.Lock()
	_, ok := p.states[state]
	delete(p.states, state)
	p.mu.Unlock()
	if !ok {
		return "", errors.New("oidc: unknown or expired state")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", p.cfg.RedirectURL)
	form.Set("client_id", p.cfg.ClientID)
	if p.cfg.ClientSecret != "" {
		form.Set("client_secret", p.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.disco.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("oidc token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("oidc token decode: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("oidc: empty access token")
	}
	return p.userinfoSubject(ctx, tok.AccessToken)
}

func (p *OIDCProvider) userinfoSubject(ctx context.Context, accessToken string) (string, error) {
	if p.disco.UserinfoEndpoint == "" {
		return "authenticated", nil // no userinfo endpoint; treat as opaque success
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.disco.UserinfoEndpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc userinfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc userinfo: status %d", resp.StatusCode)
	}
	var ui struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ui); err != nil {
		return "", fmt.Errorf("oidc userinfo decode: %w", err)
	}
	if ui.Email != "" {
		return ui.Email, nil
	}
	if ui.Sub != "" {
		return ui.Sub, nil
	}
	return "authenticated", nil
}

// gcStatesLocked drops states older than 10 minutes. Caller holds p.mu.
func (p *OIDCProvider) gcStatesLocked() {
	cutoff := time.Now().Add(-10 * time.Minute)
	for s, t := range p.states {
		if t.Before(cutoff) {
			delete(p.states, s)
		}
	}
}

func randomState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
