package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// oidcClient implements the OpenID Connect authorization code flow.
//
// The implementation is deliberately small. It supports the
// authorization code flow with PKCE, validates the ID token signature
// against the issuer's JWKS, and nothing else. There is no implicit
// flow, no hybrid flow and no token introspection.
type oidcClient struct {
	issuer   string
	clientID string
	secret   string
	redirect string
	domains  map[string]struct{}
	http     *http.Client

	mu        sync.Mutex
	metadata  *oidcMetadata
	discovery time.Time
	states    map[string]*oidcState
}

// oidcState is a pending authorisation request.
type oidcState struct {
	nonce    string
	created  time.Time
	redirect string
}

// oidcMetadata is the subset of the provider metadata Barq uses.
type oidcMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// newOIDCClient creates a client and validates the configuration.
func newOIDCClient(config Config) (*oidcClient, error) {
	if config.OIDCIssuer == "" || config.OIDCClientID == "" {
		return nil, fmt.Errorf("auth: OIDC requires an issuer and a client id")
	}
	issuerURL, err := url.Parse(config.OIDCIssuer)
	if err != nil {
		return nil, fmt.Errorf("auth: invalid OIDC issuer: %w", err)
	}
	if issuerURL.Scheme != "https" {
		return nil, fmt.Errorf("auth: the OIDC issuer must use https")
	}
	domains := make(map[string]struct{}, len(config.AllowedDomains))
	for _, domain := range config.AllowedDomains {
		domains[strings.ToLower(domain)] = struct{}{}
	}
	client := &oidcClient{
		issuer:   strings.TrimRight(config.OIDCIssuer, "/"),
		clientID: config.OIDCClientID,
		secret:   config.OIDCClientSecret,
		redirect: config.OIDCRedirectURL,
		domains:  domains,
		http:     &http.Client{Timeout: 20 * time.Second},
		states:   make(map[string]*oidcState),
	}
	return client, nil
}

// AuthorizationURL builds the URL that starts the login flow.
func (c *oidcClient) AuthorizationURL(ctx context.Context, redirectAfter string) (string, error) {
	metadata, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	state := randomToken()
	nonce := randomToken()
	c.mu.Lock()
	c.states[state] = &oidcState{nonce: nonce, created: time.Now(), redirect: redirectAfter}
	// Prune stale states so that the map cannot grow without bound.
	for key, value := range c.states {
		if time.Since(value.created) > 15*time.Minute {
			delete(c.states, key)
		}
	}
	c.mu.Unlock()

	query := url.Values{}
	query.Set("response_type", "code")
	query.Set("client_id", c.clientID)
	query.Set("redirect_uri", c.redirect)
	query.Set("scope", "openid email profile")
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("code_challenge", challengeFor(nonce))
	query.Set("code_challenge_method", "S256")
	return metadata.AuthorizationEndpoint + "?" + query.Encode(), nil
}

// Exchange completes the login flow and returns the verified claims.
func (c *oidcClient) Exchange(ctx context.Context, code, state string) (*oidcClaims, string, error) {
	c.mu.Lock()
	pending, ok := c.states[state]
	if ok {
		delete(c.states, state)
	}
	c.mu.Unlock()
	if !ok {
		return nil, "", fmt.Errorf("auth: unknown or expired OIDC state")
	}
	if time.Since(pending.created) > 15*time.Minute {
		return nil, "", fmt.Errorf("auth: the OIDC request expired")
	}

	metadata, err := c.discover(ctx)
	if err != nil {
		return nil, "", err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.redirect)
	form.Set("client_id", c.clientID)
	form.Set("code_verifier", pending.nonce)
	if c.secret != "" {
		form.Set("client_secret", c.secret)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, metadata.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return nil, "", fmt.Errorf("auth: the OIDC token endpoint returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokens struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&tokens); err != nil {
		return nil, "", err
	}
	if tokens.IDToken == "" {
		return nil, "", fmt.Errorf("auth: the OIDC provider did not return an id token")
	}

	claims, err := c.verifyIDToken(ctx, metadata, tokens.IDToken, pending.nonce)
	if err != nil {
		return nil, "", err
	}
	return claims, pending.redirect, nil
}

// oidcClaims are the verified claims from an ID token.
type oidcClaims struct {
	Subject       string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name"`
	Issuer        string   `json:"iss"`
	Audience      audience `json:"aud"`
	Nonce         string   `json:"nonce"`
	ExpiresAt     int64    `json:"exp"`
}

// audience is the aud claim, which may be a string or an array.
type audience []string

// UnmarshalJSON accepts both the string and the array form.
func (a *audience) UnmarshalJSON(data []byte) error {
	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		*a = list
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}
	*a = audience{single}
	return nil
}

// contains reports whether the audience list contains a value.
func (a audience) contains(value string) bool {
	for _, entry := range a {
		if entry == value {
			return true
		}
	}
	return false
}

// discover fetches and caches the provider metadata.
func (c *oidcClient) discover(ctx context.Context) (*oidcMetadata, error) {
	c.mu.Lock()
	if c.metadata != nil && time.Since(c.discovery) < time.Hour {
		metadata := c.metadata
		c.mu.Unlock()
		return metadata, nil
	}
	c.mu.Unlock()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: the OIDC discovery document returned %d", response.StatusCode)
	}
	metadata := &oidcMetadata{}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(metadata); err != nil {
		return nil, err
	}
	if metadata.AuthorizationEndpoint == "" || metadata.TokenEndpoint == "" {
		return nil, fmt.Errorf("auth: the OIDC discovery document is incomplete")
	}
	c.mu.Lock()
	c.metadata = metadata
	c.discovery = time.Now()
	c.mu.Unlock()
	return metadata, nil
}

// verifyIDToken validates the signature and the standard claims of an
// ID token.
func (c *oidcClient) verifyIDToken(ctx context.Context, metadata *oidcMetadata, token, nonce string) (*oidcClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("auth: malformed id token")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("auth: malformed id token header: %w", err)
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("auth: malformed id token payload: %w", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("auth: malformed id token signature: %w", err)
	}

	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, err
	}
	// The algorithm is pinned. Accepting the algorithm advertised by
	// the token is the classic JWT confusion vulnerability.
	if header.Algorithm != "RS256" && header.Algorithm != "ES256" {
		return nil, fmt.Errorf("auth: unsupported id token algorithm %q", header.Algorithm)
	}

	keys, err := c.fetchKeys(ctx, metadata)
	if err != nil {
		return nil, err
	}
	signingInput := parts[0] + "." + parts[1]
	if err := verifySignature(signingInput, signature, keys, header.KeyID, header.Algorithm); err != nil {
		return nil, err
	}

	claims := &oidcClaims{}
	if err := json.Unmarshal(payloadBytes, claims); err != nil {
		return nil, err
	}
	if claims.Issuer != c.issuer {
		return nil, fmt.Errorf("auth: the id token was issued by %q, expected %q", claims.Issuer, c.issuer)
	}
	if !claims.Audience.contains(c.clientID) {
		return nil, fmt.Errorf("auth: the id token audience does not include this client")
	}
	if claims.ExpiresAt > 0 && time.Now().After(time.Unix(claims.ExpiresAt, 0)) {
		return nil, fmt.Errorf("auth: the id token has expired")
	}
	if claims.Nonce != "" && claims.Nonce != nonce {
		return nil, fmt.Errorf("auth: the id token nonce does not match")
	}
	if len(c.domains) > 0 {
		host := claims.Email
		if index := strings.Index(host, "@"); index >= 0 {
			host = host[index+1:]
		}
		if _, ok := c.domains[strings.ToLower(host)]; !ok {
			return nil, fmt.Errorf("auth: the account domain is not allowed")
		}
	}
	return claims, nil
}

// fetchKeys retrieves the provider's JSON web key set.
func (c *oidcClient) fetchKeys(ctx context.Context, metadata *oidcMetadata) ([]jsonWebKey, error) {
	if metadata.JWKSURI == "" {
		return nil, fmt.Errorf("auth: the OIDC provider does not publish a key set")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadata.JWKSURI, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: the OIDC key set returned %d", response.StatusCode)
	}
	var document struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&document); err != nil {
		return nil, err
	}
	return document.Keys, nil
}

// challengeFor derives the PKCE challenge for a verifier.
func challengeFor(verifier string) string {
	sum := sha256Sum([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum)
}

// randomState produces a random state value.
func randomState() string {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return randomToken()
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}
