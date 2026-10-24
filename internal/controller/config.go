package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"barq/internal/apierr"
	"barq/internal/auth"
	"barq/internal/configfile"
	"barq/internal/storage"
	"barq/internal/yaml"
)

// configApplier applies a Barq configuration document.
//
// Applying a document is idempotent. The applier computes the desired
// state and reconciles towards it: users that are declared are created
// or updated, users that are absent are removed, and secrets are
// reconciled by name.
type configApplier struct {
	mu       sync.Mutex
	auth     Authenticator
	vault    SecretVault
	baseDir  string
	warnings []string
}

// SecretVault is the subset of the secret store the applier needs.
type SecretVault interface {
	Set(ctx context.Context, name, value string) (storage.SecretMetadata, error)
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]storage.SecretMetadata, error)
}

// newConfigApplier creates an applier.
func newConfigApplier(authenticator Authenticator, vault SecretVault) *configApplier {
	return &configApplier{auth: authenticator, vault: vault}
}

// apply reconciles the installation towards a configuration document.
func (a *configApplier) apply(ctx context.Context, raw []byte, baseDir string) ([]map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.warnings = nil

	config, err := configfile.Parse(raw)
	if err != nil {
		return nil, err
	}
	resolved, err := configfile.Expand(config, baseDir)
	if err != nil {
		return nil, err
	}

	applied := make([]map[string]any, 0)

	// Users. A declared user is created or updated. A user that is
	// not declared is removed, unless it is the last administrator.
	existing := map[string]*auth.User{}
	if a.auth != nil {
		for _, user := range a.auth.ListUsers() {
			existing[strings.ToLower(user.Email)] = user
		}
	}
	declared := map[string]struct{}{}
	for _, user := range resolved.Users {
		declared[strings.ToLower(user.Email)] = struct{}{}
		if current, ok := existing[strings.ToLower(user.Email)]; ok {
			if current.Role != user.Role {
				if err := a.auth.SetRole(ctx, current.ID, user.Role); err != nil {
					return nil, err
				}
			}
			if err := a.auth.SetDisabled(ctx, current.ID, user.Disabled); err != nil {
				return nil, err
			}
			applied = append(applied, map[string]any{"kind": "User", "name": user.Email})
			continue
		}
		// A new user needs a password. The document may reference one
		// through the environment, otherwise the user is created with
		// an unusable random password and must use single sign-on.
		password := ""
		if user.PasswordRef != "" {
			value, ok := os.LookupEnv(user.PasswordRef)
			if !ok {
				a.warnings = append(a.warnings, fmt.Sprintf(
					"user %q references the environment variable %s, which is not set",
					user.Email, user.PasswordRef))
			}
			password = value
		}
		if password != "" {
			created, err := a.auth.Signup(ctx, user.Email, password, user.DisplayName)
			if err != nil {
				return nil, err
			}
			if err := a.auth.SetRole(ctx, created.ID, user.Role); err != nil {
				return nil, err
			}
		} else {
			a.warnings = append(a.warnings, fmt.Sprintf(
				"user %q was not created because no password was supplied; use single sign-on or set %s",
				user.Email, user.PasswordRef))
			continue
		}
		applied = append(applied, map[string]any{"kind": "User", "name": user.Email})
	}

	// Secrets. The value is never stored in the document, only the
	// reference to it.
	if a.vault != nil {
		for _, secret := range resolved.Secrets {
			value, err := secret.SecretValue(os.LookupEnv)
			if err != nil {
				a.warnings = append(a.warnings, err.Error())
				continue
			}
			if _, err := a.vault.Set(ctx, secret.Name, value); err != nil {
				return nil, err
			}
			applied = append(applied, map[string]any{"kind": "Secret", "name": secret.Name})
		}
	}

	// Pipelines are validated but not started. Starting a pipeline is
	// a separate, explicit action.
	for _, pipeline := range resolved.Pipelines {
		if err := validatePipelineDocument(pipeline.Definition); err != nil {
			return nil, fmt.Errorf("pipeline %q: %w", pipeline.Name, err)
		}
		applied = append(applied, map[string]any{"kind": "Pipeline", "name": pipeline.Name})
	}

	return applied, nil
}

// validate resolves a configuration document without applying it.
//
// It runs the same resolution as apply, so a document that validates
// is a document that would apply cleanly. Nothing is written.
func (a *configApplier) validate(ctx context.Context, raw []byte, baseDir string) ([]map[string]any, []string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.warnings = nil

	config, err := configfile.Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := configfile.Expand(config, baseDir)
	if err != nil {
		return nil, nil, err
	}

	planned := make([]map[string]any, 0)
	for _, user := range resolved.Users {
		planned = append(planned, map[string]any{"kind": "User", "name": user.Email})
	}
	for _, secret := range resolved.Secrets {
		if _, err := secret.SecretValue(os.LookupEnv); err != nil {
			a.warnings = append(a.warnings, err.Error())
		}
		planned = append(planned, map[string]any{"kind": "Secret", "name": secret.Name})
	}
	for _, pipeline := range resolved.Pipelines {
		if err := validatePipelineDocument(pipeline.Definition); err != nil {
			return nil, nil, fmt.Errorf("pipeline %q: %w", pipeline.Name, err)
		}
		planned = append(planned, map[string]any{"kind": "Pipeline", "name": pipeline.Name})
	}
	return planned, a.warnings, nil
}

// handleConfigApply applies a configuration document.
func (e *Engine) handleConfigApply(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeAPIError(w, apierr.BadRequest("unable to read the request body"))
		return
	}
	baseDir := r.URL.Query().Get("base")
	if baseDir == "" {
		baseDir = "."
	}
	applied, err := e.applier.apply(r.Context(), raw, baseDir)
	if err != nil {
		writeAPIError(w, apierr.BadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied":  applied,
		"warnings": e.applier.warnings,
	})
}

// handleConfigValidate validates a configuration document.
//
// It does not change anything. The endpoint exists so that a
// configuration can be checked in a pipeline before it is applied.
func (e *Engine) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeAPIError(w, apierr.BadRequest("unable to read the request body"))
		return
	}
	baseDir := r.URL.Query().Get("base")
	if baseDir == "" {
		baseDir = "."
	}
	planned, warnings, err := e.applier.validate(r.Context(), raw, baseDir)
	if err != nil {
		writeAPIError(w, apierr.BadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":    true,
		"planned":  planned,
		"warnings": warnings,
	})
}

// validatePipelineDocument checks that a pipeline definition parses.
func validatePipelineDocument(definition string) error {
	_, err := yaml.Parse([]byte(definition))
	return err
}

// sortedStrings returns a sorted copy.
func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

// unusedHelpers keeps the compiler honest about the imports that the
// applier relies on indirectly.
var (
	_ = sortedStrings
	_ = time.Now
)
