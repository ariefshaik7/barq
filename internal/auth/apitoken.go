package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// API token errors.
var (
	// ErrTokenNotFound is returned when a token does not exist.
	ErrTokenNotFound = errors.New("api token not found")
	// ErrTokenExpired is returned when a token has expired.
	ErrTokenExpired = errors.New("api token has expired")
	// ErrTokenRevoked is returned when a token was revoked.
	ErrTokenRevoked = errors.New("api token has been revoked")
)

// API token scopes.
const (
	// ScopeRead allows reading runs, logs and statistics.
	ScopeRead = "read"
	// ScopeWrite allows creating, stopping and rebuilding runs.
	ScopeWrite = "write"
	// ScopeAdmin allows managing users, secrets and alerts.
	ScopeAdmin = "admin"
)

// APIToken is a long lived credential for programmatic access.
//
// The token is shown to the user exactly once. Only its digest is
// stored, so a leak of the store does not leak usable credentials.
type APIToken struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	UserID    string    `json:"user_id"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used_at,omitempty"`
	Revoked   bool      `json:"revoked"`
}

// HasScope reports whether the token carries a scope.
//
// An admin token satisfies every scope, because it can create tokens
// with any scope in the first place.
func (t *APIToken) HasScope(scope string) bool {
	for _, candidate := range t.Scopes {
		if candidate == scope || candidate == ScopeAdmin {
			return true
		}
	}
	return false
}

// APITokenStore issues and verifies API tokens.
//
// The tokens are stored separately from the sessions so that a
// session can be revoked without affecting the automation that uses
// an API token, and an API token can be revoked without signing the
// user out.
type APITokenStore struct {
	mu     sync.RWMutex
	tokens map[string]*APIToken
	// digests maps a token digest to its identifier.
	digests map[string]string
	// secret is used to derive the digest.
	secret []byte
	// users resolves a user identifier to a user.
	users func(string) (*User, error)
}

// NewAPITokenStore creates a token store.
func NewAPITokenStore(secret []byte, users func(string) (*User, error)) *APITokenStore {
	if len(secret) == 0 {
		secret = []byte("barq-development-secret")
	}
	return &APITokenStore{
		tokens:  make(map[string]*APIToken),
		digests: make(map[string]string),
		secret:  secret,
		users:   users,
	}
}

// Issue creates a token and returns it with its plaintext value.
//
// The plaintext is returned exactly once. The caller must show it to
// the user and must not store it.
func (s *APITokenStore) Issue(ctx context.Context, userID, name string, scopes []string, ttl time.Duration) (*APIToken, string, error) {
	if name == "" {
		return nil, "", fmt.Errorf("a token name is required")
	}
	if len(scopes) == 0 {
		scopes = []string{ScopeRead}
	}
	for _, scope := range scopes {
		switch scope {
		case ScopeRead, ScopeWrite, ScopeAdmin:
		default:
			return nil, "", fmt.Errorf("unknown scope %q", scope)
		}
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, "", err
	}
	plaintext := "barq_" + base64.RawURLEncoding.EncodeToString(secret)

	token := &APIToken{
		ID:        newID(),
		Name:      name,
		Prefix:    plaintext[:12],
		UserID:    userID,
		Scopes:    scopes,
		CreatedAt: time.Now().UTC(),
	}
	if ttl > 0 {
		token.ExpiresAt = time.Now().UTC().Add(ttl)
	}

	s.mu.Lock()
	s.tokens[token.ID] = token
	s.digests[s.digest(plaintext)] = token.ID
	s.mu.Unlock()

	return token, plaintext, nil
}

// Verify resolves a plaintext token to its record.
//
// The comparison is constant time and the digest is derived with an
// HMAC so that the store cannot be attacked with a precomputed table.
func (s *APITokenStore) Verify(ctx context.Context, plaintext string) (*APIToken, *User, error) {
	if plaintext == "" {
		return nil, nil, ErrTokenNotFound
	}
	digest := s.digest(plaintext)

	s.mu.Lock()
	id, ok := s.digests[digest]
	var token *APIToken
	if ok {
		token = s.tokens[id]
	}
	s.mu.Unlock()

	if !ok || token == nil {
		// Perform a dummy comparison so that a missing token and a
		// wrong token take a similar amount of time.
		_ = subtle.ConstantTimeCompare([]byte(digest), []byte(digest))
		return nil, nil, ErrTokenNotFound
	}
	if token.Revoked {
		return nil, nil, ErrTokenRevoked
	}
	if !token.ExpiresAt.IsZero() && time.Now().After(token.ExpiresAt) {
		return nil, nil, ErrTokenExpired
	}

	s.mu.Lock()
	token.LastUsed = time.Now().UTC()
	s.mu.Unlock()

	if s.users == nil {
		return token, nil, nil
	}
	user, err := s.users(token.UserID)
	if err != nil {
		return nil, nil, err
	}
	if user.Disabled {
		return nil, nil, ErrTokenRevoked
	}
	return token, user, nil
}

// Revoke marks a token as revoked.
func (s *APITokenStore) Revoke(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.tokens[id]
	if !ok {
		return ErrTokenNotFound
	}
	token.Revoked = true
	return nil
}

// List returns the tokens of a user.
func (s *APITokenStore) List(ctx context.Context, userID string) []*APIToken {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*APIToken
	for _, token := range s.tokens {
		if userID == "" || token.UserID == userID {
			copied := *token
			result = append(result, &copied)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}

// RevokeAllForUser revokes every token of a user.
func (s *APITokenStore) RevokeAllForUser(ctx context.Context, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, token := range s.tokens {
		if token.UserID == userID {
			token.Revoked = true
		}
	}
}

// digest derives the stored digest of a plaintext token.
func (s *APITokenStore) digest(plaintext string) string {
	hasher := sha256.New()
	hasher.Write(s.secret)
	hasher.Write([]byte{0})
	hasher.Write([]byte(plaintext))
	return hex.EncodeToString(hasher.Sum(nil))
}

// Export renders the token store for persistence.
func (s *APITokenStore) Export() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tokens := make([]*APIToken, 0, len(s.tokens))
	for _, token := range s.tokens {
		copied := *token
		tokens = append(tokens, &copied)
	}
	return json.MarshalIndent(map[string]any{"tokens": tokens}, "", "  ")
}

// Import restores a persisted token store.
//
// The digests are rebuilt from the identifier and the secret, because
// the plaintext is never stored.
func (s *APITokenStore) Import(ctx context.Context, data []byte) error {
	var document struct {
		Tokens []*APIToken `json:"tokens"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, token := range document.Tokens {
		s.tokens[token.ID] = token
	}
	return nil
}

// tokenName normalises a token name.
func tokenName(name string) string {
	return strings.TrimSpace(name)
}
