package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"barq/internal/storage"
)

func newStore(t *testing.T) *storage.Local {
	t.Helper()
	store, err := storage.NewLocal(filepath.Join(t.TempDir(), "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newProvider(t *testing.T) *Provider {
	t.Helper()
	provider, err := New(Config{
		Store:       newStore(t),
		Prefix:      "barq",
		AllowSignup: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestSignupCreatesTheFirstAdministrator(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()

	user, err := provider.Signup(ctx, "admin@example.com", "correct horse battery staple", "Admin")
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != RoleAdmin {
		t.Fatalf("the first account must be an administrator, got %q", user.Role)
	}
	if user.PasswordHash != "" {
		t.Fatal("the password hash must never be returned")
	}
}

func TestSignupRejectsWeakPasswords(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()
	for _, password := range []string{"short", "alllettersonly", "12345678901234"} {
		if _, err := provider.Signup(ctx, "user@example.com", password, ""); err == nil {
			t.Fatalf("expected the password %q to be rejected", password)
		}
	}
}

func TestSignupRejectsDuplicateEmails(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()
	if _, err := provider.Signup(ctx, "user@example.com", "correct horse battery staple", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Signup(ctx, "USER@example.com", "correct horse battery staple", ""); !errors.Is(err, ErrUserExists) {
		t.Fatalf("expected a duplicate account error, got %v", err)
	}
}

func TestSigninAndAuthenticate(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()
	if _, err := provider.Signup(ctx, "user@example.com", "correct horse battery staple", "User"); err != nil {
		t.Fatal(err)
	}
	user, session, err := provider.Signin(ctx, "user@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.ID == "" {
		t.Fatal("expected a session")
	}
	resolved, err := provider.Authenticate(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != user.ID {
		t.Fatalf("expected the session to resolve to the user, got %q", resolved.ID)
	}
}

func TestSigninRejectsWrongPassword(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()
	if _, err := provider.Signup(ctx, "user@example.com", "correct horse battery staple", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.Signin(ctx, "user@example.com", "wrong password entirely"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected an invalid credentials error, got %v", err)
	}
	// An unknown account must produce the same error, so that the
	// endpoint cannot be used to enumerate accounts.
	if _, _, err := provider.Signin(ctx, "nobody@example.com", "correct horse battery staple"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected an invalid credentials error, got %v", err)
	}
}

func TestSignoutRevokesTheSession(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()
	if _, err := provider.Signup(ctx, "user@example.com", "correct horse battery staple", ""); err != nil {
		t.Fatal(err)
	}
	_, session, err := provider.Signin(ctx, "user@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Signout(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Authenticate(session.ID); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("expected the session to be revoked, got %v", err)
	}
}

func TestPasswordHashIsSaltedAndVerifiable(t *testing.T) {
	first, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two hashes of the same password must differ because the salt is random")
	}
	if !VerifyPassword("correct horse battery staple", first) {
		t.Fatal("expected the password to verify")
	}
	if VerifyPassword("wrong password entirely", first) {
		t.Fatal("expected the wrong password to fail")
	}
}

func TestPasswordHashDoesNotContainThePlaintext(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("the hash contains the plaintext")
	}
}

func TestRoleHierarchy(t *testing.T) {
	if !RoleAdmin.AtLeast(RoleViewer) {
		t.Fatal("an administrator must satisfy the viewer role")
	}
	if !RoleBuilder.AtLeast(RoleViewer) {
		t.Fatal("a builder must satisfy the viewer role")
	}
	if RoleViewer.AtLeast(RoleBuilder) {
		t.Fatal("a viewer must not satisfy the builder role")
	}
	if RoleViewer.AtLeast(RoleAdmin) {
		t.Fatal("a viewer must not satisfy the administrator role")
	}
}

func TestRequireRole(t *testing.T) {
	provider := newProvider(t)
	viewer := &User{Role: RoleViewer}
	if err := provider.RequireRole(viewer, RoleViewer); err != nil {
		t.Fatal(err)
	}
	if err := provider.RequireRole(viewer, RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected a forbidden error, got %v", err)
	}
	if err := provider.RequireRole(nil, RoleViewer); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("expected an authentication error, got %v", err)
	}
}

func TestDisabledAccountCannotSignIn(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()
	user, err := provider.Signup(ctx, "user@example.com", "correct horse battery staple", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SetDisabled(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.Signin(ctx, "user@example.com", "correct horse battery staple"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("expected a disabled account error, got %v", err)
	}
}

func TestSetRoleAndListUsers(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()
	user, err := provider.Signup(ctx, "user@example.com", "correct horse battery staple", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.SetRole(ctx, user.ID, RoleBuilder); err != nil {
		t.Fatal(err)
	}
	users := provider.ListUsers()
	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(users))
	}
	if users[0].Role != RoleBuilder {
		t.Fatalf("expected the builder role, got %q", users[0].Role)
	}
	if users[0].PasswordHash != "" {
		t.Fatal("the list must not expose password hashes")
	}
}

func TestChangePasswordRevokesSessions(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()
	user, err := provider.Signup(ctx, "user@example.com", "correct horse battery staple", "")
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := provider.Signin(ctx, "user@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ChangePassword(ctx, user.ID, "correct horse battery staple", "a brand new long password"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Authenticate(session.ID); !errors.Is(err, ErrNotAuthenticated) {
		t.Fatal("changing the password must revoke the existing sessions")
	}
	if _, _, err := provider.Signin(ctx, "user@example.com", "a brand new long password"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteUser(t *testing.T) {
	provider := newProvider(t)
	ctx := context.Background()
	user, err := provider.Signup(ctx, "user@example.com", "correct horse battery staple", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.DeleteUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.Signin(ctx, "user@example.com", "correct horse battery staple"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("the deleted account must not be able to sign in")
	}
}

func TestUsersSurviveARestart(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	first, err := New(Config{Store: store, Prefix: "barq", AllowSignup: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Signup(ctx, "user@example.com", "correct horse battery staple", "User"); err != nil {
		t.Fatal(err)
	}
	_, session, err := first.Signin(ctx, "user@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}

	second, err := New(Config{Store: store, Prefix: "barq", AllowSignup: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Authenticate(session.ID); err != nil {
		t.Fatalf("the session must survive a restart, got %v", err)
	}
	if _, _, err := second.Signin(ctx, "user@example.com", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
}
