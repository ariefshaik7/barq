// Package config loads and validates Barq runtime configuration.
//
// Configuration is intentionally environment-only: the controller is a
// single static binary with no config file format, which keeps the attack
// surface small and matches the "zero external dependency" constraint.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Mode selects the execution backend and the guarantees Barq makes.
type Mode string

const (
	// ModeProduction runs real worker Pods against a durable object store.
	ModeProduction Mode = "production"
	// ModeLocal runs real worker Pods against a local (development only)
	// object store. Data is not durable across controller restarts.
	ModeLocal Mode = "local"
	// ModeSimulator runs everything in-process. It is explicitly synthetic
	// and must never be used to claim a build succeeded.
	ModeSimulator Mode = "simulator"
)

// ObjectBackend selects the durable object store implementation.
type ObjectBackend string

const (
	// BackendS3 is a durable, S3-compatible object store (AWS S3, MinIO,
	// Cloudflare R2, ...).
	BackendS3 ObjectBackend = "s3"
	// BackendLocal is a leader-local development object store. It is NOT
	// durable and NOT S3-compatible.
	BackendLocal ObjectBackend = "local"
)

// Config is the fully validated controller configuration.
type Config struct {
	// Mode and ObjectBackend select the runtime behaviour.
	Mode          Mode
	ObjectBackend ObjectBackend

	// Namespace is the namespace workers are created in.
	Namespace string
	// Identity is the stable identity of this controller instance. It must
	// survive restarts of the same Pod; the Pod UID is preferred.
	Identity string
	// LeaseName is the coordination.k8s.io Lease used for leader election.
	LeaseName string
	// LeaderURL is the base URL other replicas use to reach this instance
	// when it is the leader (for example http://10.42.0.7:8080).
	LeaderURL string
	// Port is the HTTP listen port.
	Port int

	// HotRoot is the local hot-tier directory for run state and logs.
	HotRoot string
	// ObjectPrefix namespaces every object written by this controller.
	ObjectPrefix string
	// LocalObjectRoot is the directory backing the local object store.
	LocalObjectRoot string
	// PublicURL is the externally reachable base URL of the controller,
	// used when generating local object relay URLs.
	PublicURL string

	// S3 configuration.
	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3SessionToken    string
	S3PathStyle       bool

	// APIToken authenticates the control-plane API. Required in production.
	APIToken string
	// AuthRequired forces authentication on outside production mode.
	AuthRequired bool
	// AllowSignup permits local account creation.
	AllowSignup bool
	// SecureCookies marks session cookies as Secure.
	SecureCookies bool
	// SessionTTL is how long a session remains valid.
	SessionTTL time.Duration
	// MaxConcurrentRuns is the global limit on the number of runs
	// that may execute at the same time. Zero means unlimited.
	MaxConcurrentRuns int
	// OIDC single sign-on configuration.
	OIDCIssuer         string
	OIDCClientID       string
	OIDCClientSecret   string
	OIDCRedirectURL    string
	OIDCAllowedDomains []string
	// GitHubIntegration enables commit status reporting.
	GitHubIntegration bool
	GitHubOwner       string
	GitHubRepository  string
	// MasterKeyText is the raw master key before decoding. It is kept
	// so that the value can be redacted from logs.
	MasterKeyText string
	// MasterKey encrypts the cluster secret store. Required in production.
	MasterKey []byte

	// Webhook configuration.
	GitHubWebhookSecret string
	GitHubAPIToken      string
	GitHubAPIBase       string
	GitLabWebhookToken  string
	GitLabAPIToken      string
	GitLabAPIBase       string
	AllowedBranches     map[string]struct{}

	// PluginRuntimeImage is the image providing Barq built-in plugins.
	PluginRuntimeImage string

	// Runtime tuning.
	MaxAttempts    int
	PollInterval   time.Duration
	LeaseDuration  time.Duration
	LeaseRenewTime time.Duration
	ShutdownGrace  time.Duration
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	cfg := Config{
		Mode:                Mode(envOr("BARQ_MODE", string(ModeSimulator))),
		ObjectBackend:       ObjectBackend(envOr("BARQ_OBJECT_BACKEND", string(BackendLocal))),
		Namespace:           envOr("BARQ_NAMESPACE", "default"),
		Identity:            envOr("BARQ_IDENTITY", ""),
		LeaseName:           envOr("BARQ_LEASE_NAME", "barq-leader"),
		LeaderURL:           envOr("BARQ_LEADER_URL", ""),
		Port:                envInt("BARQ_PORT", 8080),
		HotRoot:             envOr("BARQ_HOT_ROOT", defaultHotRoot()),
		ObjectPrefix:        envOr("BARQ_OBJECT_PREFIX", "barq"),
		LocalObjectRoot:     envOr("BARQ_LOCAL_OBJECT_ROOT", defaultLocalObjectRoot()),
		PublicURL:           envOr("BARQ_PUBLIC_URL", ""),
		S3Endpoint:          envOr("BARQ_S3_ENDPOINT", "https://s3.amazonaws.com"),
		S3Region:            envOr("BARQ_S3_REGION", "us-east-1"),
		S3Bucket:            envOr("BARQ_S3_BUCKET", ""),
		S3AccessKeyID:       os.Getenv("BARQ_S3_ACCESS_KEY_ID"),
		S3SecretAccessKey:   os.Getenv("BARQ_S3_SECRET_ACCESS_KEY"),
		S3SessionToken:      os.Getenv("BARQ_S3_SESSION_TOKEN"),
		S3PathStyle:         envBool("BARQ_S3_PATH_STYLE", true),
		APIToken:            os.Getenv("BARQ_API_TOKEN"),
		GitHubWebhookSecret: os.Getenv("BARQ_GITHUB_WEBHOOK_SECRET"),
		GitHubAPIToken:      os.Getenv("BARQ_GITHUB_API_TOKEN"),
		GitHubAPIBase:       envOr("BARQ_GITHUB_API_BASE", "https://api.github.com"),
		GitLabWebhookToken:  os.Getenv("BARQ_GITLAB_WEBHOOK_TOKEN"),
		GitLabAPIToken:      os.Getenv("BARQ_GITLAB_API_TOKEN"),
		GitLabAPIBase:       envOr("BARQ_GITLAB_API_BASE", "https://gitlab.com"),
		PluginRuntimeImage:  envOr("BARQ_PLUGIN_RUNTIME_IMAGE", "barq/plugin-runtime:1.0.0"),
		MaxAttempts:         envInt("BARQ_MAX_ATTEMPTS", 3),
		PollInterval:        envDuration("BARQ_POLL_INTERVAL", 2*time.Second),
		LeaseDuration:       envDuration("BARQ_LEASE_DURATION", 10*time.Second),
		LeaseRenewTime:      envDuration("BARQ_LEASE_RENEW_TIME", 3*time.Second),
		ShutdownGrace:       envDuration("BARQ_SHUTDOWN_GRACE", 20*time.Second),
		AuthRequired:        envBool("BARQ_AUTH_REQUIRED", false),
		AllowSignup:         envBool("BARQ_ALLOW_SIGNUP", false),
		SecureCookies:       envBool("BARQ_SECURE_COOKIES", false),
		SessionTTL:          envDuration("BARQ_SESSION_TTL", 12*time.Hour),
		MaxConcurrentRuns:   envInt("BARQ_MAX_CONCURRENT_RUNS", 0),
		OIDCIssuer:          os.Getenv("BARQ_OIDC_ISSUER"),
		OIDCClientID:        os.Getenv("BARQ_OIDC_CLIENT_ID"),
		OIDCClientSecret:    os.Getenv("BARQ_OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:     os.Getenv("BARQ_OIDC_REDIRECT_URL"),
		OIDCAllowedDomains:  splitList(os.Getenv("BARQ_OIDC_ALLOWED_DOMAINS")),
		GitHubIntegration:   envBool("BARQ_GITHUB_INTEGRATION", false),
		GitHubOwner:         os.Getenv("BARQ_GITHUB_OWNER"),
		GitHubRepository:    os.Getenv("BARQ_GITHUB_REPOSITORY"),
		MasterKeyText:       os.Getenv("BARQ_MASTER_KEY"),
	}

	if cfg.Identity == "" {
		cfg.Identity = podUID()
	}
	if cfg.Identity == "" {
		host, err := os.Hostname()
		if err != nil {
			return Config{}, fmt.Errorf("determine controller identity: %w", err)
		}
		cfg.Identity = host
	}
	if ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if trimmed := strings.TrimSpace(string(ns)); trimmed != "" {
			cfg.Namespace = trimmed
		}
	}
	var err error
	if cfg.AllowedBranches, err = parseBranchAllowList(os.Getenv("BARQ_ALLOWED_BRANCHES")); err != nil {
		return Config{}, err
	}
	if cfg.MasterKey, err = loadMasterKey(); err != nil {
		return Config{}, err
	}

	switch cfg.Mode {
	case ModeProduction:
		if cfg.ObjectBackend != BackendS3 {
			return Config{}, fmt.Errorf("production mode requires BARQ_OBJECT_BACKEND=s3")
		}
		if cfg.S3Bucket == "" {
			return Config{}, fmt.Errorf("production mode requires BARQ_S3_BUCKET")
		}
		if cfg.S3AccessKeyID == "" || cfg.S3SecretAccessKey == "" {
			return Config{}, fmt.Errorf("production mode requires S3 credentials")
		}
		if cfg.APIToken == "" {
			return Config{}, fmt.Errorf("production mode requires BARQ_API_TOKEN")
		}
		if len(cfg.MasterKey) != 32 {
			return Config{}, fmt.Errorf("production mode requires a 32 byte BARQ_MASTER_KEY")
		}
	case ModeLocal, ModeSimulator:
		if cfg.ObjectBackend == BackendS3 && cfg.S3Bucket == "" {
			return Config{}, fmt.Errorf("BARQ_OBJECT_BACKEND=s3 requires BARQ_S3_BUCKET")
		}
	default:
		return Config{}, fmt.Errorf("unknown BARQ_MODE %q", cfg.Mode)
	}

	if cfg.Mode == ModeSimulator {
		cfg.ObjectBackend = BackendLocal
	}
	return cfg, nil
}

func podUID() string {
	raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/../uid")
	if err == nil {
		if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
			return trimmed
		}
	}
	return strings.TrimSpace(os.Getenv("BARQ_POD_UID"))
}

func loadMasterKey() ([]byte, error) {
	if path := os.Getenv("BARQ_MASTER_KEY_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read BARQ_MASTER_KEY_FILE: %w", err)
		}
		return decodeKey(raw)
	}
	raw := os.Getenv("BARQ_MASTER_KEY")
	if raw == "" {
		return nil, nil
	}
	return decodeKey([]byte(raw))
}

func decodeKey(raw []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(raw))
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(trimmed) == 32 {
		return []byte(trimmed), nil
	}
	return nil, fmt.Errorf("master key must be 32 raw bytes or base64 of 32 bytes")
}

func parseBranchAllowList(raw string) (map[string]struct{}, error) {
	result := make(map[string]struct{})
	for _, branch := range strings.Split(raw, ",") {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			continue
		}
		if !strings.HasPrefix(branch, "refs/heads/") {
			branch = "refs/heads/" + branch
		}
		result[branch] = struct{}{}
	}
	return result, nil
}

// splitList splits a comma separated environment variable.
func splitList(raw string) []string {
	var result []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}

// defaultHotRoot returns a user-writable directory for the hot tier when
// BARQ_HOT_ROOT is not set. In production the operator should set it
// explicitly; in local/simulator mode we fall back to the user cache dir
// so that `go run ./cmd/barq` works without sudo.
func defaultHotRoot() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "barq", "ebs")
	}
	return filepath.Join(os.TempDir(), "barq-ebs")
}

// defaultLocalObjectRoot returns a user-writable directory for the local
// object store when BARQ_LOCAL_OBJECT_ROOT is not set.
func defaultLocalObjectRoot() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "barq", "ebs", "objects")
	}
	return filepath.Join(os.TempDir(), "barq-ebs-objects")
}
