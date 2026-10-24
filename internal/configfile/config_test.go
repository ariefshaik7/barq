package configfile

import (
	"os"
	"path/filepath"
	"testing"

	"barq/internal/auth"
)

const sampleDocument = `
version: 1
kind: BarqConfig
meta:
  name: production
  environment: prod
  owner: platform
server:
  mode: production
  object_backend: s3
  replicas: 3
  max_attempts: 5
  allowed_branches: ["main", "release"]
auth:
  signup_enabled: false
  oidc:
    issuer: "https://accounts.example.com"
    client_id: "barq"
    allowed_domains: ["example.com"]
users:
  - email: "admin@example.com"
    display_name: "Admin"
    role: admin
    password_ref: "ADMIN_PASSWORD"
  - email: "dev@example.com"
    role: builder
secrets:
  - name: NPM_TOKEN
    from_env: "NPM_TOKEN"
pipelines:
  - name: release
    path: pipelines/release.yaml
    active: true
`

func TestParse(t *testing.T) {
	config, err := Parse([]byte(sampleDocument))
	if err != nil {
		t.Fatal(err)
	}
	if config.Version != 1 {
		t.Fatalf("expected version 1, got %d", config.Version)
	}
	if config.Kind != "BarqConfig" {
		t.Fatalf("expected the BarqConfig kind, got %q", config.Kind)
	}
	if config.Meta.Name != "production" {
		t.Fatalf("expected the production metadata, got %+v", config.Meta)
	}
	if config.Server.Replicas != 3 {
		t.Fatalf("expected 3 replicas, got %d", config.Server.Replicas)
	}
	if len(config.Users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(config.Users))
	}
	if len(config.Secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(config.Secrets))
	}
	if len(config.Pipes) != 1 {
		t.Fatalf("expected 1 pipeline, got %d", len(config.Pipes))
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	document := "version: 1\nkind: BarqConfig\nserver:\n  not_a_real_field: true\n"
	if _, err := Parse([]byte(document)); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
}

func TestExpand(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pipelines"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipelines", "release.yaml"),
		[]byte("name: release\nsteps:\n  - id: build\n    image: alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config, err := Parse([]byte(sampleDocument))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Expand(config, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(resolved.Users))
	}
	// The users must be sorted by email so that the output is stable.
	if resolved.Users[0].Email != "admin@example.com" {
		t.Fatalf("expected the users to be sorted, got %+v", resolved.Users)
	}
	if resolved.Users[0].Role != auth.RoleAdmin {
		t.Fatalf("expected the admin role, got %q", resolved.Users[0].Role)
	}
	if resolved.Users[1].Role != auth.RoleBuilder {
		t.Fatalf("expected the builder role, got %q", resolved.Users[1].Role)
	}
	if len(resolved.Pipelines) != 1 {
		t.Fatalf("expected 1 pipeline, got %d", len(resolved.Pipelines))
	}
	if resolved.Pipelines[0].Definition == "" {
		t.Fatal("expected the pipeline definition to be inlined")
	}
}

func TestExpandRejectsUnknownRole(t *testing.T) {
	config, err := Parse([]byte(`
version: 1
kind: BarqConfig
users:
  - email: "a@example.com"
    role: superuser
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Expand(config, "."); err == nil {
		t.Fatal("expected an error for an unknown role")
	}
}

func TestExpandRejectsDuplicateUsers(t *testing.T) {
	config, err := Parse([]byte(`
version: 1
kind: BarqConfig
users:
  - email: "a@example.com"
  - email: "A@example.com"
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Expand(config, "."); err == nil {
		t.Fatal("expected an error for duplicate users")
	}
}

func TestExpandRejectsAFutureVersion(t *testing.T) {
	config, err := Parse([]byte("version: 99\nkind: BarqConfig\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Expand(config, "."); err == nil {
		t.Fatal("expected an error for an unsupported version")
	}
}

func TestSecretValueResolvesFromTheEnvironment(t *testing.T) {
	secret := ResolvedSecret{Name: "TOKEN", FromEnv: "TEST_TOKEN"}
	value, err := secret.SecretValue(func(key string) (string, bool) {
		if key == "TEST_TOKEN" {
			return "resolved", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if value != "resolved" {
		t.Fatalf("expected the resolved value, got %q", value)
	}
}

func TestSecretValueFailsWhenTheEnvironmentIsUnset(t *testing.T) {
	secret := ResolvedSecret{Name: "TOKEN", FromEnv: "MISSING_TOKEN"}
	if _, err := secret.SecretValue(func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("expected an error for a missing environment variable")
	}
}

func TestSecretValueNeverReadsAFile(t *testing.T) {
	// A secret must never be sourced from a file path supplied by the
	// document. Only the environment or an explicit literal is allowed.
	secret := ResolvedSecret{Name: "TOKEN", FromRef: "/etc/passwd"}
	if _, err := secret.SecretValue(func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("expected an error for an unresolvable reference")
	}
}
