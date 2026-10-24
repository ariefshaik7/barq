// Package configfile implements Barq's configuration as code.
//
// A Barq configuration is a YAML document that describes the settings
// of a Barq installation: the controller, its users, its credentials
// and its pipelines. It is declarative, diffable, reviewable and
// appliable, in the same way that a Kubernetes manifest or a Terraform
// configuration is.
//
// The document is intentionally separate from the pipeline definition
// language. A pipeline describes what to build. A Barq configuration
// describes the installation itself.
package configfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"barq/internal/auth"
	"barq/internal/yaml"
)

// Version is the configuration schema version.
const Version = 1

// Config is a complete Barq configuration.
type Config struct {
	Version int           `json:"version"`
	Kind    string        `json:"kind"`
	Meta    Metadata      `json:"meta"`
	Server  ServerConfig  `json:"server"`
	Auth    AuthConfig    `json:"auth"`
	Users   []UserConfig  `json:"users,omitempty"`
	Secrets []SecretSpec  `json:"secrets,omitempty"`
	Pipes   []PipelineRef `json:"pipelines,omitempty"`
}

// Metadata describes the configuration itself.
type Metadata struct {
	Name        string            `json:"name"`
	Environment string            `json:"environment,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	CreatedAt   time.Time         `json:"created_at,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// ServerConfig describes the controller settings.
type ServerConfig struct {
	Mode            string            `json:"mode,omitempty"`
	ObjectBackend   string            `json:"object_backend,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	LeaseName       string            `json:"lease_name,omitempty"`
	Replicas        int               `json:"replicas,omitempty"`
	ObjectPrefix    string            `json:"object_prefix,omitempty"`
	PublicURL       string            `json:"public_url,omitempty"`
	PluginRuntime   string            `json:"plugin_runtime_image,omitempty"`
	MaxAttempts     int               `json:"max_attempts,omitempty"`
	PollInterval    string            `json:"poll_interval,omitempty"`
	LeaseDuration   string            `json:"lease_duration,omitempty"`
	SessionTTL      string            `json:"session_ttl,omitempty"`
	AllowedBranches []string          `json:"allowed_branches,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
}

// AuthConfig describes the identity settings.
type AuthConfig struct {
	SignupEnabled bool                `json:"signup_enabled,omitempty"`
	OIDC          *OIDCConfig         `json:"oidc,omitempty"`
	Providers     []string            `json:"providers,omitempty"`
	Labels        map[string]string   `json:"labels,omitempty"`
	Extra         map[string][]string `json:"extra,omitempty"`
}

// OIDCConfig describes the single sign-on settings.
type OIDCConfig struct {
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"client_id"`
	ClientSecretRef string   `json:"client_secret_ref,omitempty"`
	RedirectURL     string   `json:"redirect_url,omitempty"`
	AllowedDomains  []string `json:"allowed_domains,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
}

// UserConfig declares a user account.
type UserConfig struct {
	Email       string   `json:"email"`
	DisplayName string   `json:"display_name,omitempty"`
	Role        string   `json:"role,omitempty"`
	PasswordRef string   `json:"password_ref,omitempty"`
	OIDCSubject string   `json:"oidc_subject,omitempty"`
	Groups      []string `json:"groups,omitempty"`
	Disabled    bool     `json:"disabled,omitempty"`
}

// SecretSpec declares a secret. The value is never stored in the
// document. It is referenced.
type SecretSpec struct {
	Name    string `json:"name"`
	FromEnv string `json:"from_env,omitempty"`
	FromRef string `json:"from_ref,omitempty"`
	Literal string `json:"literal,omitempty"`
	Note    string `json:"note,omitempty"`
}

// PipelineRef points at a pipeline definition.
type PipelineRef struct {
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Inline string `json:"inline,omitempty"`
	Active bool   `json:"active,omitempty"`
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	config, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

// Parse decodes a configuration document.
//
// The document uses the same YAML subset as pipelines, so it is parsed
// with the shared parser and then decoded with unknown field
// rejection.
func Parse(raw []byte) (*Config, error) {
	config := &Config{}
	if err := yaml.Decode(raw, config); err != nil {
		return nil, err
	}
	return config, nil
}

// Expand resolves a configuration against a base directory, producing
// the concrete settings the controller should run with.
func Expand(config *Config, baseDir string) (*Resolved, error) {
	if config == nil {
		return nil, fmt.Errorf("no configuration supplied")
	}
	if config.Version == 0 {
		return nil, fmt.Errorf("the configuration must declare version: %d", Version)
	}
	if config.Version > Version {
		return nil, fmt.Errorf("the configuration declares version %d but this build supports %d", config.Version, Version)
	}
	if config.Kind != "" && config.Kind != "BarqConfig" {
		return nil, fmt.Errorf("unsupported configuration kind %q", config.Kind)
	}

	resolved := &Resolved{
		Config: config,
		Users:  make([]ResolvedUser, 0, len(config.Users)),
	}

	seenUsers := map[string]struct{}{}
	for _, user := range config.Users {
		email := strings.ToLower(strings.TrimSpace(user.Email))
		if email == "" {
			return nil, fmt.Errorf("a user is missing an email address")
		}
		if _, duplicate := seenUsers[email]; duplicate {
			return nil, fmt.Errorf("duplicate user %q", email)
		}
		seenUsers[email] = struct{}{}
		role := auth.RoleViewer
		switch strings.ToLower(user.Role) {
		case "", "viewer":
			role = auth.RoleViewer
		case "builder":
			role = auth.RoleBuilder
		case "admin":
			role = auth.RoleAdmin
		default:
			return nil, fmt.Errorf("user %q has an unknown role %q", email, user.Role)
		}
		resolved.Users = append(resolved.Users, ResolvedUser{
			Email:       email,
			DisplayName: user.DisplayName,
			Role:        role,
			PasswordRef: user.PasswordRef,
			OIDCSubject: user.OIDCSubject,
			Groups:      user.Groups,
			Disabled:    user.Disabled,
		})
	}
	sort.Slice(resolved.Users, func(i, j int) bool { return resolved.Users[i].Email < resolved.Users[j].Email })

	resolved.Secrets = make([]ResolvedSecret, 0, len(config.Secrets))
	for _, secret := range config.Secrets {
		if secret.Name == "" {
			return nil, fmt.Errorf("a secret is missing a name")
		}
		if secret.FromEnv == "" && secret.FromRef == "" && secret.Literal == "" {
			return nil, fmt.Errorf("secret %q has no source", secret.Name)
		}
		resolved.Secrets = append(resolved.Secrets, ResolvedSecret{
			Name:    secret.Name,
			FromEnv: secret.FromEnv,
			FromRef: secret.FromRef,
			Literal: secret.Literal,
		})
	}

	resolved.Pipelines = make([]ResolvedPipeline, 0, len(config.Pipes))
	for _, pipeline := range config.Pipes {
		if pipeline.Name == "" {
			return nil, fmt.Errorf("a pipeline is missing a name")
		}
		definition := pipeline.Inline
		if definition == "" && pipeline.Path != "" {
			path := pipeline.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(baseDir, path)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read pipeline %q: %w", pipeline.Name, err)
			}
			definition = string(raw)
		}
		if definition == "" {
			return nil, fmt.Errorf("pipeline %q has neither inline nor path", pipeline.Name)
		}
		resolved.Pipelines = append(resolved.Pipelines, ResolvedPipeline{
			Name:       pipeline.Name,
			Definition: definition,
			Active:     pipeline.Active,
		})
	}

	return resolved, nil
}

// Resolved is a configuration with every reference expanded.
type Resolved struct {
	Config    *Config
	Users     []ResolvedUser
	Secrets   []ResolvedSecret
	Pipelines []ResolvedPipeline
}

// ResolvedUser is a user with its references resolved.
type ResolvedUser struct {
	Email       string
	DisplayName string
	Role        auth.Role
	PasswordRef string
	OIDCSubject string
	Groups      []string
	Disabled    bool
}

// ResolvedSecret is a secret with its references resolved.
type ResolvedSecret struct {
	Name    string
	FromEnv string
	FromRef string
	Literal string
}

// ResolvedPipeline is a pipeline with its definition inlined.
type ResolvedPipeline struct {
	Name       string
	Definition string
	Active     bool
}

// SecretValue resolves the value of a secret from the environment or
// from an explicit literal.
//
// A secret is never read from a file path supplied by the document.
// The reference names an environment variable, which keeps the
// document free of credentials and keeps the credentials out of disk.
func (s ResolvedSecret) SecretValue(lookup func(string) (string, bool)) (string, error) {
	switch {
	case s.FromEnv != "":
		value, ok := lookup(s.FromEnv)
		if !ok || value == "" {
			return "", fmt.Errorf("secret %q: the environment variable %s is not set", s.Name, s.FromEnv)
		}
		return value, nil
	case s.Literal != "":
		return s.Literal, nil
	default:
		return "", fmt.Errorf("secret %q has no resolvable source", s.Name)
	}
}
