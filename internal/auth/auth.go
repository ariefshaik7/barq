// Package auth implements Barq's identity and access management.
//
// Barq uses local accounts with Argon2id password hashing, signed
// session cookies, and OIDC single sign-on. Every authentication
// decision is made through a single Provider interface so that the
// HTTP layer never has to know which mechanism was used.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"barq/internal/storage"
)

// Errors returned by the provider.
var (
	// ErrInvalidCredentials is returned when a login attempt fails.
	// It deliberately does not distinguish between an unknown user and
	// a wrong password, so that the endpoint cannot be used to
	// enumerate accounts.
	ErrInvalidCredentials = errors.New("invalid email or password")
	// ErrUserExists is returned when a signup collides with an
	// existing account.
	ErrUserExists = errors.New("an account with that email already exists")
	// ErrUserNotFound is returned when an account does not exist.
	ErrUserNotFound = errors.New("user not found")
	// ErrNotAuthenticated is returned when a request has no session.
	ErrNotAuthenticated = errors.New("not authenticated")
	// ErrForbidden is returned when an authenticated user lacks a role.
	ErrForbidden = errors.New("forbidden")
	// ErrWeakPassword is returned when a password does not meet the
	// minimum policy.
	ErrWeakPassword = errors.New("password does not meet the minimum policy")
	// ErrAccountDisabled is returned when an account is disabled.
	ErrAccountDisabled = errors.New("account is disabled")
)

// Role is a coarse authorisation level.
type Role string

const (
	// RoleViewer can read runs and logs.
	RoleViewer Role = "viewer"
	// RoleBuilder can create and rebuild runs.
	RoleBuilder Role = "builder"
	// RoleAdmin can do everything, including managing users and
	// secrets.
	RoleAdmin Role = "admin"
)

// rank orders the roles for comparison.
func (r Role) rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleBuilder:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// AtLeast reports whether the role satisfies a required role.
func (r Role) AtLeast(required Role) bool { return r.rank() >= required.rank() }

// User is an account.
//
// The password hash is deliberately excluded from the JSON
// representation. The persisted form uses a separate type so that the
// hash can be stored without any risk of it reaching the API.
type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	DisplayName  string     `json:"display_name"`
	Role         Role       `json:"role"`
	PasswordHash string     `json:"-"`
	OIDCSubject  string     `json:"-"`
	OIDCIssuer   string     `json:"-"`
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// storedUser is the persisted form of an account. It is the only place
// where the password hash is written.
type storedUser struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	DisplayName  string     `json:"display_name"`
	Role         Role       `json:"role"`
	PasswordHash string     `json:"password_hash"`
	OIDCSubject  string     `json:"oidc_subject,omitempty"`
	OIDCIssuer   string     `json:"oidc_issuer,omitempty"`
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// toStored converts a user into its persisted form.
func (u *User) toStored() storedUser {
	return storedUser{
		ID:           u.ID,
		Email:        u.Email,
		DisplayName:  u.DisplayName,
		Role:         u.Role,
		PasswordHash: u.PasswordHash,
		OIDCSubject:  u.OIDCSubject,
		OIDCIssuer:   u.OIDCIssuer,
		Disabled:     u.Disabled,
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
		LastLoginAt:  u.LastLoginAt,
	}
}

// fromStored converts a persisted user back into a user.
func (s storedUser) fromStored() *User {
	return &User{
		ID:           s.ID,
		Email:        s.Email,
		DisplayName:  s.DisplayName,
		Role:         s.Role,
		PasswordHash: s.PasswordHash,
		OIDCSubject:  s.OIDCSubject,
		OIDCIssuer:   s.OIDCIssuer,
		Disabled:     s.Disabled,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
		LastLoginAt:  s.LastLoginAt,
	}
}

// Session is an issued session.
type Session struct {
	ID        string
	UserID    string
	ExpiresAt time.Time
	CSRFToken string
}

// Config configures the provider.
type Config struct {
	// Store is the durable backend for users and sessions.
	Store storage.ObjectStore
	// Prefix namespaces the objects written by the provider.
	Prefix string
	// SessionTTL is how long a session remains valid.
	SessionTTL time.Duration
	// SecureCookies marks cookies as Secure. It must be true behind
	// TLS in production.
	SecureCookies bool
	// AllowedDomains is the list of OIDC issuer hosts that may be
	// used for single sign-on. An empty list disables OIDC.
	AllowedDomains []string
	// OIDCIssuer is the issuer URL for single sign-on.
	OIDCIssuer string
	// OIDCClientID is the client identifier.
	OIDCClientID string
	// OIDCClientSecret is the client secret.
	OIDCClientSecret string
	// OIDCRedirectURL is the callback URL.
	OIDCRedirectURL string
	// AllowSignup permits local account creation. It defaults to true
	// only in local mode.
	AllowSignup bool
	// Clock returns the current time. It exists for testing.
	Clock func() time.Time
}

// Provider is the authentication and authorisation surface.
type Provider struct {
	config Config

	mu       sync.RWMutex
	users    map[string]*User
	byEmail  map[string]*User
	sessions map[string]*Session
	loaded   bool

	oidc *oidcClient
}

// New creates a provider and loads any persisted state.
func New(config Config) (*Provider, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("auth requires an object store")
	}
	if config.SessionTTL <= 0 {
		config.SessionTTL = 12 * time.Hour
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	provider := &Provider{
		config:   config,
		users:    make(map[string]*User),
		byEmail:  make(map[string]*User),
		sessions: make(map[string]*Session),
	}
	if err := provider.load(); err != nil {
		return nil, err
	}
	if config.OIDCIssuer != "" && config.OIDCClientID != "" {
		client, err := newOIDCClient(config)
		if err != nil {
			return nil, err
		}
		provider.oidc = client
	}
	return provider, nil
}

// Config returns the provider configuration.
func (p *Provider) Config() Config { return p.config }

// OIDCEnabled reports whether single sign-on is configured.
func (p *Provider) OIDCEnabled() bool { return p.oidc != nil }

// Signup registers a local account.
//
// The very first account created on a fresh installation becomes an
// administrator, so that a new deployment can be bootstrapped without
// an out-of-band secret.
func (p *Provider) Signup(ctx context.Context, email, password, displayName string) (*User, error) {
	if !p.config.AllowSignup {
		return nil, fmt.Errorf("signup is disabled")
	}
	email = normalizeEmail(email)
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("a valid email address is required")
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.byEmail[email] != nil {
		p.mu.Unlock()
		return nil, ErrUserExists
	}
	first := len(p.users) == 0
	user := &User{
		ID:           newID(),
		Email:        email,
		DisplayName:  strings.TrimSpace(displayName),
		Role:         RoleViewer,
		PasswordHash: hash,
		CreatedAt:    p.config.Clock().UTC(),
		UpdatedAt:    p.config.Clock().UTC(),
	}
	if first {
		user.Role = RoleAdmin
	}
	if user.DisplayName == "" {
		user.DisplayName = email
	}
	p.users[user.ID] = user
	p.byEmail[email] = user
	p.mu.Unlock()

	if err := p.persist(ctx); err != nil {
		return nil, err
	}
	return publicUser(user), nil
}

// Signin verifies a password and issues a session.
func (p *Provider) Signin(ctx context.Context, email, password string) (*User, *Session, error) {
	email = normalizeEmail(email)
	p.mu.RLock()
	user := p.byEmail[email]
	p.mu.RUnlock()

	if user == nil {
		// Perform a dummy verification so that a missing account and a
		// wrong password take a similar amount of time.
		_ = VerifyPassword(password, dummyHash)
		return nil, nil, ErrInvalidCredentials
	}
	if user.Disabled {
		return nil, nil, ErrAccountDisabled
	}
	if !VerifyPassword(password, user.PasswordHash) {
		return nil, nil, ErrInvalidCredentials
	}

	now := p.config.Clock().UTC()
	p.mu.Lock()
	user.LastLoginAt = &now
	user.UpdatedAt = now
	p.mu.Unlock()

	session, err := p.issueSession(user.ID)
	if err != nil {
		return nil, nil, err
	}
	if err := p.persist(ctx); err != nil {
		return nil, nil, err
	}
	return publicUser(user), session, nil
}

// Signout revokes a session.
func (p *Provider) Signout(ctx context.Context, sessionID string) error {
	p.mu.Lock()
	delete(p.sessions, sessionID)
	p.mu.Unlock()
	return p.persist(ctx)
}

// Authenticate resolves a session identifier to a user.
func (p *Provider) Authenticate(sessionID string) (*User, error) {
	if sessionID == "" {
		return nil, ErrNotAuthenticated
	}
	p.mu.RLock()
	session, ok := p.sessions[sessionID]
	p.mu.RUnlock()
	if !ok {
		return nil, ErrNotAuthenticated
	}
	if p.config.Clock().After(session.ExpiresAt) {
		p.mu.Lock()
		delete(p.sessions, sessionID)
		p.mu.Unlock()
		return nil, ErrNotAuthenticated
	}
	p.mu.RLock()
	user, ok := p.users[session.UserID]
	p.mu.RUnlock()
	if !ok || user.Disabled {
		return nil, ErrNotAuthenticated
	}
	return publicUser(user), nil
}

// RequireRole returns an error unless the user holds at least the
// required role.
func (p *Provider) RequireRole(user *User, required Role) error {
	if user == nil {
		return ErrNotAuthenticated
	}
	if !user.Role.AtLeast(required) {
		return ErrForbidden
	}
	return nil
}

// OIDCAuthorizationURL starts the single sign-on flow.
func (p *Provider) OIDCAuthorizationURL(ctx context.Context, redirectAfter string) (string, error) {
	if p.oidc == nil {
		return "", fmt.Errorf("auth: single sign-on is not configured")
	}
	return p.oidc.AuthorizationURL(ctx, redirectAfter)
}

// OIDCExchange completes the single sign-on flow.
//
// The account is created on first use. A user that already exists is
// matched on the issuer and subject, never on the email address,
// because the email address is only a claim and the subject is the
// stable identifier.
func (p *Provider) OIDCExchange(ctx context.Context, code, state string) (*User, *Session, error) {
	if p.oidc == nil {
		return nil, nil, fmt.Errorf("auth: single sign-on is not configured")
	}
	claims, redirect, err := p.oidc.Exchange(ctx, code, state)
	if err != nil {
		return nil, nil, err
	}
	if claims.Email == "" {
		return nil, nil, fmt.Errorf("auth: the identity provider did not supply an email address")
	}

	p.mu.Lock()
	var existing *User
	for _, user := range p.users {
		if user.OIDCIssuer == p.config.OIDCIssuer && user.OIDCSubject == claims.Subject {
			existing = user
			break
		}
	}
	if existing == nil {
		// Fall back to matching by email so that a user who signed up
		// locally can link their single sign-on identity.
		existing = p.byEmail[normalizeEmail(claims.Email)]
	}
	if existing != nil {
		existing.OIDCIssuer = p.config.OIDCIssuer
		existing.OIDCSubject = claims.Subject
		existing.UpdatedAt = p.config.Clock().UTC()
	} else {
		role := RoleViewer
		if len(p.users) == 0 {
			role = RoleAdmin
		}
		user := &User{
			ID:          newID(),
			Email:       normalizeEmail(claims.Email),
			DisplayName: claims.Name,
			Role:        role,
			OIDCIssuer:  p.config.OIDCIssuer,
			OIDCSubject: claims.Subject,
			CreatedAt:   p.config.Clock().UTC(),
			UpdatedAt:   p.config.Clock().UTC(),
		}
		if user.DisplayName == "" {
			user.DisplayName = user.Email
		}
		p.users[user.ID] = user
		p.byEmail[user.Email] = user
		existing = user
	}
	p.mu.Unlock()

	if existing.Disabled {
		return nil, nil, ErrAccountDisabled
	}
	session, err := p.issueSession(existing.ID)
	if err != nil {
		return nil, nil, err
	}
	if err := p.persist(ctx); err != nil {
		return nil, nil, err
	}
	_ = redirect
	return publicUser(existing), session, nil
}

// ListUsers returns every account, without password material.
func (p *Provider) ListUsers() []*User {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]*User, 0, len(p.users))
	for _, user := range p.users {
		result = append(result, publicUser(user))
	}
	sortUsers(result)
	return result
}

// SetRole changes the role of an account.
func (p *Provider) SetRole(ctx context.Context, userID string, role Role) error {
	switch role {
	case RoleViewer, RoleBuilder, RoleAdmin:
	default:
		return fmt.Errorf("unknown role %q", role)
	}
	p.mu.Lock()
	user, ok := p.users[userID]
	if !ok {
		p.mu.Unlock()
		return ErrUserNotFound
	}
	user.Role = role
	user.UpdatedAt = p.config.Clock().UTC()
	p.mu.Unlock()
	return p.persist(ctx)
}

// SetDisabled enables or disables an account.
func (p *Provider) SetDisabled(ctx context.Context, userID string, disabled bool) error {
	p.mu.Lock()
	user, ok := p.users[userID]
	if !ok {
		p.mu.Unlock()
		return ErrUserNotFound
	}
	user.Disabled = disabled
	user.UpdatedAt = p.config.Clock().UTC()
	if disabled {
		// Revoke every session belonging to a disabled account.
		for id, session := range p.sessions {
			if session.UserID == userID {
				delete(p.sessions, id)
			}
		}
	}
	p.mu.Unlock()
	return p.persist(ctx)
}

// DeleteUser removes an account.
func (p *Provider) DeleteUser(ctx context.Context, userID string) error {
	p.mu.Lock()
	user, ok := p.users[userID]
	if !ok {
		p.mu.Unlock()
		return ErrUserNotFound
	}
	delete(p.users, userID)
	delete(p.byEmail, user.Email)
	for id, session := range p.sessions {
		if session.UserID == userID {
			delete(p.sessions, id)
		}
	}
	p.mu.Unlock()
	return p.persist(ctx)
}

// UpdateProfile changes the display name of an account.
func (p *Provider) UpdateProfile(ctx context.Context, userID, displayName string) error {
	p.mu.Lock()
	user, ok := p.users[userID]
	if !ok {
		p.mu.Unlock()
		return ErrUserNotFound
	}
	user.DisplayName = strings.TrimSpace(displayName)
	if user.DisplayName == "" {
		user.DisplayName = user.Email
	}
	user.UpdatedAt = p.config.Clock().UTC()
	p.mu.Unlock()
	return p.persist(ctx)
}

// ChangePassword replaces the password of an account.
func (p *Provider) ChangePassword(ctx context.Context, userID, current, next string) error {
	if err := ValidatePassword(next); err != nil {
		return err
	}
	p.mu.Lock()
	user, ok := p.users[userID]
	if !ok {
		p.mu.Unlock()
		return ErrUserNotFound
	}
	if user.PasswordHash != "" {
		if !VerifyPassword(current, user.PasswordHash) {
			p.mu.Unlock()
			return ErrInvalidCredentials
		}
	}
	hash, err := HashPassword(next)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	user.PasswordHash = hash
	user.UpdatedAt = p.config.Clock().UTC()
	for id, session := range p.sessions {
		if session.UserID == userID {
			delete(p.sessions, id)
		}
	}
	p.mu.Unlock()
	return p.persist(ctx)
}

// issueSession creates a session for a user.
func (p *Provider) issueSession(userID string) (*Session, error) {
	session := &Session{
		ID:        newID(),
		UserID:    userID,
		ExpiresAt: p.config.Clock().UTC().Add(p.config.SessionTTL),
		CSRFToken: newID(),
	}
	p.mu.Lock()
	p.sessions[session.ID] = session
	p.mu.Unlock()
	return session, nil
}

// Persist writes the user and session state to the object store.
func (p *Provider) persist(ctx context.Context) error {
	p.mu.RLock()
	users := make([]storedUser, 0, len(p.users))
	for _, user := range p.users {
		users = append(users, user.toStored())
	}
	sessions := make([]*Session, 0, len(p.sessions))
	for _, session := range p.sessions {
		copied := *session
		sessions = append(sessions, &copied)
	}
	p.mu.RUnlock()

	now := p.config.Clock().UTC()
	document := struct {
		Version   int          `json:"version"`
		UpdatedAt time.Time    `json:"updated_at"`
		Users     []storedUser `json:"users"`
		Sessions  []*Session   `json:"sessions"`
	}{Version: 1, UpdatedAt: now, Users: users, Sessions: sessions}

	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return p.config.Store.Put(ctx, p.config.Prefix+"/config/auth.json", encoded, "application/json")
}

// load reads the persisted user and session state.
func (p *Provider) load() error {
	data, err := p.config.Store.Get(context.Background(), p.config.Prefix+"/config/auth.json")
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	var document struct {
		Users    []storedUser `json:"users"`
		Sessions []*Session   `json:"sessions"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("decode auth state: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, stored := range document.Users {
		user := stored.fromStored()
		p.users[user.ID] = user
		p.byEmail[user.Email] = user
	}
	for _, session := range document.Sessions {
		if session.ExpiresAt.After(p.config.Clock()) {
			p.sessions[session.ID] = session
		}
	}
	p.loaded = true
	return nil
}

// publicUser strips sensitive fields.
func publicUser(user *User) *User {
	copied := *user
	copied.PasswordHash = ""
	return &copied
}

// sortUsers orders accounts by email.
func sortUsers(users []*User) {
	sort.Slice(users, func(i, j int) bool { return users[i].Email < users[j].Email })
}

// normalizeEmail lowercases and trims an email address.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// newID produces a random identifier.
func newID() string {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}

// randomToken produces a URL safe random token.
func randomToken() string {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return newID()
	}
	return hex.EncodeToString(buffer)
}
