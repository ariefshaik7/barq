package controller

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"barq/internal/apierr"
	"barq/internal/auth"
)

// sessionCookieName is the name of the session cookie.
const sessionCookieName = "barq_session"

// contextKey is the type used for request scoped values.
type contextKey string

const (
	// userContextKey holds the authenticated user.
	userContextKey contextKey = "barq.user"
	// sessionContextKey holds the active session.
	sessionContextKey contextKey = "barq.session"
)

// Authenticator is the subset of the auth provider the API needs.
type Authenticator interface {
	Config() auth.Config
	Signup(ctx context.Context, email, password, displayName string) (*auth.User, error)
	Signin(ctx context.Context, email, password string) (*auth.User, *auth.Session, error)
	Signout(ctx context.Context, sessionID string) error
	Authenticate(sessionID string) (*auth.User, error)
	RequireRole(user *auth.User, required auth.Role) error
	ListUsers() []*auth.User
	SetRole(ctx context.Context, userID string, role auth.Role) error
	SetDisabled(ctx context.Context, userID string, disabled bool) error
	DeleteUser(ctx context.Context, userID string) error
	UpdateProfile(ctx context.Context, userID, displayName string) error
	ChangePassword(ctx context.Context, userID, current, next string) error
	OIDCEnabled() bool
	OIDCAuthorizationURL(ctx context.Context, redirectAfter string) (string, error)
	OIDCExchange(ctx context.Context, code, state string) (*auth.User, *auth.Session, error)
}

// sessionTTL is how long a session cookie remains valid.
const sessionTTL = 12 * time.Hour

// handleLogin authenticates a user and issues a session.
func (e *Engine) handleLogin(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	user, session, err := e.Auth.Signin(r.Context(), request.Email, request.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	e.setSessionCookie(w, r, session)
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

// handleSignup registers a new account.
func (e *Engine) handleSignup(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	user, err := e.Auth.Signup(r.Context(), request.Email, request.Password, request.DisplayName)
	if err != nil {
		if errors.Is(err, auth.ErrUserExists) {
			writeError(w, http.StatusConflict, "an account with that email already exists")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A newly created account is signed in immediately.
	_, session, err := e.Auth.Signin(r.Context(), request.Email, request.Password)
	if err != nil {
		writeJSON(w, http.StatusCreated, map[string]any{"user": user})
		return
	}
	e.setSessionCookie(w, r, session)
	writeJSON(w, http.StatusCreated, map[string]any{"user": user})
}

// handleLogout revokes the current session.
func (e *Engine) handleLogout(w http.ResponseWriter, r *http.Request) {
	if session, ok := r.Context().Value(sessionContextKey).(*auth.Session); ok {
		_ = e.Auth.Signout(r.Context(), session.ID)
	}
	e.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// handleSession returns the current user.
//
// It is a public endpoint so that the UI can ask "who am I?" before it
// has a session. It resolves the session itself rather than relying on
// the authentication middleware.
func (e *Engine) handleSession(w http.ResponseWriter, r *http.Request) {
	if e.Auth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"user": nil})
		return
	}
	sessionID := e.sessionID(r)
	if sessionID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"user": nil})
		return
	}
	user, err := e.Auth.Authenticate(sessionID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"user": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

// handleOIDCStart begins the single sign-on flow.
func (e *Engine) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if !e.Auth.OIDCEnabled() {
		writeError(w, http.StatusNotFound, "single sign-on is not configured")
		return
	}
	redirect := r.URL.Query().Get("redirect")
	target, err := e.Auth.OIDCAuthorizationURL(r.Context(), redirect)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleOIDCCallback completes the single sign-on flow.
func (e *Engine) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if !e.Auth.OIDCEnabled() {
		writeError(w, http.StatusNotFound, "single sign-on is not configured")
		return
	}
	if oauthError := r.URL.Query().Get("error"); oauthError != "" {
		writeError(w, http.StatusUnauthorized, oauthError)
		return
	}
	user, session, err := e.Auth.OIDCExchange(r.Context(), r.URL.Query().Get("code"), r.URL.Query().Get("state"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	e.setSessionCookie(w, r, session)
	if redirect := r.URL.Query().Get("redirect"); redirect != "" && strings.HasPrefix(redirect, "/") {
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

// handleListUsers returns the account list. It requires the admin role.
func (e *Engine) handleListUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"users": e.Auth.ListUsers()})
}

// handleUpdateUser changes a role or disables an account.
func (e *Engine) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	var request struct {
		Role     *string `json:"role,omitempty"`
		Disabled *bool   `json:"disabled,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if request.Role != nil {
		if err := e.Auth.SetRole(r.Context(), userID, auth.Role(*request.Role)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if request.Disabled != nil {
		if err := e.Auth.SetDisabled(r.Context(), userID, *request.Disabled); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleDeleteUser removes an account.
func (e *Engine) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := e.Auth.DeleteUser(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleProfile updates the current user's profile.
func (e *Engine) handleProfile(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(userContextKey).(*auth.User)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var request struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := e.Auth.UpdateProfile(r.Context(), user.ID, request.DisplayName); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// handleChangePassword changes the current user's password.
func (e *Engine) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(userContextKey).(*auth.User)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var request struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := e.Auth.ChangePassword(r.Context(), user.ID, request.Current, request.Next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	e.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "password changed"})
}

// handleListTokens returns the API tokens of the current user.
func (e *Engine) handleListTokens(w http.ResponseWriter, r *http.Request) {
	if e.Tokens == nil {
		writeAPIError(w, apierr.Unavailable("API tokens are not configured"))
		return
	}
	user, ok := r.Context().Value(userContextKey).(*auth.User)
	if !ok {
		writeAPIError(w, apierr.Unauthorized("authentication is required"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": e.Tokens.List(r.Context(), user.ID)})
}

// handleCreateToken issues a new API token.
//
// The plaintext is returned exactly once in the response. It is not
// stored and cannot be retrieved again.
func (e *Engine) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if e.Tokens == nil {
		writeAPIError(w, apierr.Unavailable("API tokens are not configured"))
		return
	}
	user, ok := r.Context().Value(userContextKey).(*auth.User)
	if !ok {
		writeAPIError(w, apierr.Unauthorized("authentication is required"))
		return
	}
	var request struct {
		Name     string   `json:"name"`
		Scopes   []string `json:"scopes"`
		TTLHours int      `json:"ttl_hours"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&request); err != nil {
		writeAPIError(w, apierr.BadRequest("invalid request body"))
		return
	}
	if request.Name == "" {
		writeAPIError(w, apierr.BadRequest("a token name is required"))
		return
	}
	ttl := time.Duration(request.TTLHours) * time.Hour
	token, plaintext, err := e.Tokens.Issue(r.Context(), user.ID, request.Name, request.Scopes, ttl)
	if err != nil {
		writeAPIError(w, apierr.BadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":  token,
		"secret": plaintext,
		"notice": "This value is shown once. Store it now; it cannot be retrieved again.",
	})
}

// handleRevokeToken revokes an API token.
func (e *Engine) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if e.Tokens == nil {
		writeAPIError(w, apierr.Unavailable("API tokens are not configured"))
		return
	}
	if err := e.Tokens.Revoke(r.Context(), r.PathValue("id")); err != nil {
		writeAPIError(w, apierr.NotFound("the token does not exist"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// requireAuth authenticates a request and enforces a minimum role.
//
// The pattern follows the "load then guard" idiom: the session is
// resolved first, then the permission is checked. The two are separate
// steps so that a handler can require a lower role than the one the
// route group established.
//
// Two credential types are accepted and they are deliberately
// distinguished, following the pattern that separates an interactive
// session from a programmatic token:
//
//   - A session cookie is an interactive credential. A state changing
//     request that uses it must present a CSRF token.
//   - A bearer token is a programmatic credential. It is not attached
//     automatically by a browser, so it is not vulnerable to cross
//     site request forgery and does not need a CSRF token.
func (e *Engine) requireAuth(required auth.Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if e.Auth == nil {
			// Authentication is not configured. The API is open, which
			// is only acceptable in local and simulator mode.
			next(w, r)
			return
		}
		credential := e.credential(r)
		if credential.value == "" {
			writeAPIError(w, apierr.Unauthorized("authentication is required"))
			return
		}

		var user *auth.User
		if credential.kind == credentialAPI {
			if e.Tokens == nil {
				writeAPIError(w, apierr.Unauthorized("API tokens are not enabled"))
				return
			}
			token, resolved, err := e.Tokens.Verify(r.Context(), credential.value)
			if err != nil {
				writeAPIError(w, apierr.Unauthorized("the API token is not valid"))
				return
			}
			if !token.HasScope(scopeForRole(required)) {
				writeAPIError(w, apierr.Forbidden("the API token does not carry the required scope"))
				return
			}
			user = resolved
		} else {
			resolved, err := e.Auth.Authenticate(credential.value)
			if err != nil {
				writeAPIError(w, apierr.Unauthorized("the session is not valid"))
				return
			}
			if err := e.Auth.RequireRole(resolved, required); err != nil {
				writeAPIError(w, apierr.Forbidden("the account does not have the required role"))
				return
			}
			user = resolved
			if !isSafeMethod(r.Method) && !e.verifyCSRF(r, credential.value) {
				writeAPIError(w, apierr.Forbidden("the CSRF token is missing or invalid"))
				return
			}
		}

		ctx := context.WithValue(r.Context(), userContextKey, user)
		ctx = context.WithValue(ctx, sessionContextKey, &auth.Session{ID: credential.value})
		next(w, r.WithContext(ctx))
	}
}

// scopeForRole maps a role to the API token scope it requires.
func scopeForRole(role auth.Role) string {
	switch role {
	case auth.RoleAdmin:
		return auth.ScopeAdmin
	case auth.RoleBuilder:
		return auth.ScopeWrite
	default:
		return auth.ScopeRead
	}
}

// credentialKind identifies the type of a credential.
type credentialKind string

const (
	// credentialSession is an interactive session cookie.
	credentialSession credentialKind = "session"
	// credentialAPI is a programmatic bearer token.
	credentialAPI credentialKind = "api"
)

// credential is a resolved authentication credential.
type credential struct {
	kind  credentialKind
	value string
}

// credential extracts the credential of a request.
//
// The bearer token takes precedence over the cookie, because an API
// client that happens to carry a stale cookie must not be silently
// downgraded to the session.
func (e *Engine) credential(r *http.Request) credential {
	header := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return credential{kind: credentialAPI, value: strings.TrimSpace(header[len("bearer "):])}
	}
	if token := r.Header.Get("X-Barq-Token"); token != "" {
		return credential{kind: credentialAPI, value: strings.TrimSpace(token)}
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		return credential{kind: credentialSession, value: cookie.Value}
	}
	return credential{}
}

// isSafeMethod reports whether a method may be performed without a
// CSRF token.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// sessionID extracts the session identifier from a request.
func (e *Engine) sessionID(r *http.Request) string {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	header := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[len("bearer "):])
	}
	return strings.TrimSpace(r.Header.Get("X-Barq-Token"))
}

// verifyCSRF validates the CSRF token of a state changing request.
func (e *Engine) verifyCSRF(r *http.Request, sessionID string) bool {
	// A bearer token is not attached automatically by a browser, so
	// it is not vulnerable to cross site request forgery.
	if r.Header.Get("Authorization") != "" || r.Header.Get("X-Barq-Token") != "" {
		return true
	}
	provided := r.Header.Get("X-Barq-CSRF")
	if provided == "" {
		return false
	}
	return e.csrfMatches(sessionID, provided)
}

// csrfMatches validates a CSRF token against the session.
func (e *Engine) csrfMatches(sessionID, provided string) bool {
	if e.csrf == nil {
		return true
	}
	return e.csrf.Match(sessionID, provided)
}

// setSessionCookie writes the session cookie.
func (e *Engine) setSessionCookie(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    session.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   e.secureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  session.ExpiresAt,
	}
	http.SetCookie(w, cookie)
	// The CSRF token is readable by the UI so that it can be sent
	// back in a header.
	if e.csrf != nil {
		http.SetCookie(w, &http.Cookie{
			Name:     "barq_csrf",
			Value:    e.csrf.Issue(session.ID),
			Path:     "/",
			HttpOnly: false,
			Secure:   e.secureCookies,
			SameSite: http.SameSiteLaxMode,
			Expires:  session.ExpiresAt,
		})
	}
}

// clearSessionCookie removes the session cookie.
func (e *Engine) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   e.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "barq_csrf",
		Value:    "",
		Path:     "/",
		HttpOnly: false,
		Secure:   e.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// authStatus describes the authentication state of the installation.
type authStatus struct {
	Required      bool   `json:"required"`
	SignupEnabled bool   `json:"signup_enabled"`
	OIDCEnabled   bool   `json:"oidc_enabled"`
	OIDCURL       string `json:"oidc_url,omitempty"`
}

// handleAuthStatus reports whether authentication is required.
func (e *Engine) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	status := authStatus{}
	if e.Auth != nil {
		status.Required = true
		status.SignupEnabled = e.Auth.Config().AllowSignup
		status.OIDCEnabled = e.Auth.OIDCEnabled()
	}
	writeJSON(w, http.StatusOK, status)
}

// csrfStore issues and validates CSRF tokens.
type csrfStore struct {
	mu     sync.Mutex
	tokens map[string]string
}

// newCSRFStore creates a CSRF token store.
func newCSRFStore() *csrfStore {
	return &csrfStore{tokens: make(map[string]string)}
}

// Issue creates a token for a session.
func (s *csrfStore) Issue(sessionID string) string {
	token := randomToken()
	s.mu.Lock()
	s.tokens[sessionID] = token
	// Keep the map bounded. Sessions are few, but the map must not
	// grow without limit.
	if len(s.tokens) > 10000 {
		for key := range s.tokens {
			delete(s.tokens, key)
			break
		}
	}
	s.mu.Unlock()
	return token
}

// Match validates a token against a session.
func (s *csrfStore) Match(sessionID, provided string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expected, ok := s.tokens[sessionID]
	return ok && constantTimeEqual(expected, provided)
}

// randomToken produces a random token.
func randomToken() string {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer)
}

// constantTimeEqual compares two strings in constant time.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// formatDuration renders a duration for the API.
func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}
