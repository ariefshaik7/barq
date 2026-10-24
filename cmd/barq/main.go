// Command barq is the Barq CI controller.
//
// The controller is a single static binary. It embeds the web UI, the
// plugin catalog and the worker runtime contract, and it communicates
// with Kubernetes and the object store using only the Go standard
// library.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"barq/internal/agent"
	"barq/internal/alert"
	"barq/internal/auth"
	"barq/internal/cache"
	"barq/internal/config"
	"barq/internal/controller"
	"barq/internal/dag"
	"barq/internal/health"
	"barq/internal/k8s"
	"barq/internal/queue"
	"barq/internal/stats"
	"barq/internal/storage"
	"google.golang.org/grpc"
)

// version is stamped at build time.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "barq: %v\n", err)
		os.Exit(1)
	}
}

// run wires the controller together and blocks until shutdown.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if cfg.Mode == config.ModeSimulator {
		logger.Warn("running in SIMULATOR mode: worker results are synthetic and never prove a build succeeded")
	}
	logger.Info("barq starting", "version", version, "mode", string(cfg.Mode))

	store, err := buildObjectStore(cfg, logger)
	if err != nil {
		return err
	}

	secrets := collectSecretValues(cfg)
	masker := storage.NewSecretMasker(secrets)

	tiered, err := storage.NewTiered(store, cfg.ObjectPrefix, cfg.HotRoot, masker, storage.TieredOptions{
		LogBatchSize:   1024 * 1024, // 1 MiB
		LogFlushPeriod: time.Second,
	})
	if err != nil {
		return err
	}

	manager := cache.NewManager(store, cfg.ObjectPrefix)

	var vault *storage.SecretVault
	if len(cfg.MasterKey) == 32 {
		vault, err = storage.NewSecretVault(store, cfg.MasterKey)
		if err != nil {
			return err
		}
	} else {
		logger.Warn("no master key configured: the encrypted secret store is disabled")
	}

	backend, err := buildBackend(cfg)
	if err != nil {
		return err
	}
	logger.Info("execution backend selected", "backend", backend.Name())

	// Authentication. It is mandatory in production mode.
	//
	// authenticator is declared as the controller.Authenticator interface
	// (not *auth.Provider) so that a nil interface correctly reports
	// e.Auth == nil in requireAuth. A nil *auth.Provider wrapped in an
	// interface would be non-nil and break the check.
	var authenticator controller.Authenticator
	var tokens *auth.APITokenStore
	if cfg.Mode == config.ModeProduction || cfg.AuthRequired {
		provider, err := auth.New(auth.Config{
			Store:            store,
			Prefix:           cfg.ObjectPrefix,
			SessionTTL:       cfg.SessionTTL,
			SecureCookies:    cfg.SecureCookies,
			AllowedDomains:   cfg.OIDCAllowedDomains,
			OIDCIssuer:       cfg.OIDCIssuer,
			OIDCClientID:     cfg.OIDCClientID,
			OIDCClientSecret: cfg.OIDCClientSecret,
			OIDCRedirectURL:  cfg.OIDCRedirectURL,
			AllowSignup:      cfg.AllowSignup,
		})
		if err != nil {
			return err
		}
		authenticator = provider
		tokens = auth.NewAPITokenStore([]byte(cfg.Identity), func(userID string) (*auth.User, error) {
			for _, candidate := range provider.ListUsers() {
				if candidate.ID == userID {
					return candidate, nil
				}
			}
			return nil, auth.ErrUserNotFound
		})
		logger.Info("authentication enabled", "oidc", provider.OIDCEnabled(), "signup", cfg.AllowSignup)
	} else {
		logger.Warn("authentication is disabled: anyone who can reach this port can create runs")
	}

	// Alerting.
	alerts := alert.NewManager(logger)
	alerts.SetGitHub(alert.GitHubConfig{
		Enabled:    cfg.GitHubIntegration,
		Token:      cfg.GitHubAPIToken,
		APIBase:    cfg.GitHubAPIBase,
		Owner:      cfg.GitHubOwner,
		Repository: cfg.GitHubRepository,
	})

	// Dependency health.
	monitor := health.New(version)
	registerProbes(monitor, store, backend, tiered)

	// Usage statistics.
	statistics := stats.NewManager(store, cfg.ObjectPrefix)

	// The exported metrics.
	exportedMetrics := controller.NewMetrics()

	// The task queue is the admission control point. A run that the
	// queue has not released does not execute any step.
	queueCtx, queueCancel := context.WithCancel(context.Background())
	defer queueCancel()
	taskQueue := queue.New(queueCtx, queue.Options{
		Extension:       cfg.LeaseDuration * 3,
		ProcessInterval: 100 * time.Millisecond,
		Logger:          logger,
	})

	engine := controller.New(controller.Config{
		Identity:            cfg.Identity,
		Namespace:           cfg.Namespace,
		LeaseName:           cfg.LeaseName,
		LeaderURL:           cfg.LeaderURL,
		ObjectPrefix:        cfg.ObjectPrefix,
		MaxAttempts:         cfg.MaxAttempts,
		PollInterval:        cfg.PollInterval,
		LeaseDuration:       cfg.LeaseDuration,
		LeaseRenewTime:      cfg.LeaseRenewTime,
		AllowedBranches:     cfg.AllowedBranches,
		APIToken:            cfg.APIToken,
		GitHubWebhookSecret: cfg.GitHubWebhookSecret,
		GitLabWebhookToken:  cfg.GitLabWebhookToken,
		GitHubAPIToken:      cfg.GitHubAPIToken,
		GitLabAPIToken:      cfg.GitLabAPIToken,
		GitHubAPIBase:       cfg.GitHubAPIBase,
		GitLabAPIBase:       cfg.GitLabAPIBase,
	}, backend, tiered, manager, vault, logger, dag.Options{
		PluginRuntimeImage: cfg.PluginRuntimeImage,
	}, controller.Options{
		Auth:          authenticator,
		Tokens:        tokens,
		SecureCookies: cfg.SecureCookies,
		Alerts:        alerts,
		Health:        monitor,
		Stats:         statistics,
		Queue:         taskQueue,
		Metrics:       exportedMetrics,
		PublicURL:     cfg.PublicURL,
	})

	// The statistics manager reads from the engine so that it sees
	// in-memory runs as well as persisted ones.
	statistics.SetLoader(func(ctx context.Context) ([]stats.RunSummary, error) {
		return engine.Summaries(ctx), nil
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	engine.Run(ctx)
	alerts.Start(ctx, 4)
	monitor.Start(ctx, 10*time.Second)

	// Start the gRPC agent server for external (non-Kubernetes) agents.
	// This allows Barq to work with standalone agents in addition to
	// K8s-spawned workers, matching Woodpecker's agent model.
	grpcAddr := fmt.Sprintf(":%d", cfg.Port+1000)
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		logger.Warn("failed to listen for gRPC agent server", "addr", grpcAddr, "error", err)
	} else {
		grpcServer := grpc.NewServer()
		agentServer := agent.NewServer(agent.ServerConfig{
			Engine: engine,
			Queue:  taskQueue,
			Logger: logger,
		})
		agentServer.RegisterGRPC(grpcServer)
		go func() {
			logger.Info("barq gRPC agent server listening", "addr", grpcAddr)
			if err := grpcServer.Serve(lis); err != nil {
				logger.Warn("gRPC agent server stopped", "error", err)
			}
		}()
		defer grpcServer.GracefulStop()
	}

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           engine.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// There is deliberately no WriteTimeout. Server-Sent Events
		// connections are long lived.
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("barq controller listening", "addr", server.Addr, "version", version)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	select {
	case err := <-serverErrors:
		return err
	case <-ctx.Done():
		logger.Info("shutdown requested")
	}

	alerts.Stop()
	monitor.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown failed", "error", err)
	}
	return nil
}

// registerProbes installs the dependency health checks.
func registerProbes(monitor *health.Monitor, store storage.ObjectStore, backend k8s.Backend, tiered *storage.Tiered) {
	monitor.Register(health.Probe{
		Name:        "object_store",
		Description: "The durable object store is reachable and writable.",
		Critical:    true,
		Timeout:     10 * time.Second,
		Check: func(ctx context.Context) error {
			key := "health/probe"
			if err := store.Put(ctx, key, []byte("ok"), "text/plain"); err != nil {
				return err
			}
			if _, err := store.Get(ctx, key); err != nil {
				return err
			}
			_ = store.Delete(ctx, key)
			return nil
		},
	})
	monitor.Register(health.Probe{
		Name:        "hot_tier",
		Description: "The local hot tier is writable.",
		Critical:    true,
		Timeout:     5 * time.Second,
		Check: func(ctx context.Context) error {
			return tiered.Probe(ctx)
		},
	})
	monitor.Register(health.Probe{
		Name:        "lease",
		Description: "The leader lease can be read.",
		Critical:    false,
		Timeout:     5 * time.Second,
		Check: func(ctx context.Context) error {
			_, err := backend.GetLease(ctx, "barq-leader")
			if err != nil && !k8s.IsNotFound(err) {
				return err
			}
			return nil
		},
	})
}

// buildObjectStore selects the durable object store implementation.
func buildObjectStore(cfg config.Config, logger *slog.Logger) (storage.ObjectStore, error) {
	switch cfg.ObjectBackend {
	case config.BackendS3:
		return storage.NewS3(storage.S3Config{
			Endpoint:        cfg.S3Endpoint,
			Region:          cfg.S3Region,
			Bucket:          cfg.S3Bucket,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
			SessionToken:    cfg.S3SessionToken,
			PathStyle:       cfg.S3PathStyle,
		})
	case config.BackendLocal:
		secret, err := relaySecret()
		if err != nil {
			return nil, err
		}
		public := cfg.PublicURL
		if public == "" {
			public = fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
		}
		logger.Warn("using the leader-local object store: data is NOT durable across controller restarts")
		return storage.NewLocal(cfg.LocalObjectRoot, public, secret)
	default:
		return nil, fmt.Errorf("unknown object backend %q", cfg.ObjectBackend)
	}
}

// buildBackend selects the execution backend.
func buildBackend(cfg config.Config) (k8s.Backend, error) {
	if cfg.Mode == config.ModeSimulator {
		root, err := os.MkdirTemp("", "barq-simulator-*")
		if err != nil {
			return nil, err
		}
		return k8s.NewSimulator(root)
	}
	return k8s.NewInCluster(cfg.Namespace)
}

// relaySecret derives the signing secret for the local object relay.
func relaySecret() ([]byte, error) {
	if raw := os.Getenv("BARQ_RELAY_SECRET"); raw != "" {
		decoded, err := hex.DecodeString(raw)
		if err != nil || len(decoded) < 16 {
			return nil, fmt.Errorf("BARQ_RELAY_SECRET must be at least 16 bytes of hex")
		}
		return decoded, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	// The generated secret is written to disk so that a restart inside
	// the same Pod can still verify previously issued capabilities.
	path := filepath.Join(os.TempDir(), "barq-relay-secret")
	if err := os.WriteFile(path, secret, 0o600); err == nil {
		slog.Default().Warn("relay secret generated; set BARQ_RELAY_SECRET for multi-replica deployments")
	}
	return secret, nil
}

// collectSecretValues gathers the values that must be redacted from logs.
func collectSecretValues(cfg config.Config) []string {
	values := []string{}
	for _, candidate := range []string{
		cfg.S3AccessKeyID,
		cfg.S3SecretAccessKey,
		cfg.APIToken,
		cfg.GitHubWebhookSecret,
		cfg.GitLabWebhookToken,
		cfg.GitHubAPIToken,
		cfg.GitLabAPIToken,
		cfg.OIDCClientSecret,
		cfg.MasterKeyText,
	} {
		if candidate != "" {
			values = append(values, candidate)
		}
	}
	return values
}
