// Command barq-agent is the Barq CI agent.
//
// The agent connects to a Barq controller via gRPC, registers itself,
// and pulls workflows to execute. It supports external (non-Kubernetes)
// agents for on-prem, hybrid, and edge deployments.
//
// Usage:
//
//	barq-agent --server controller:8080 --agent-id my-agent --hostname host1 --labels kubernetes=true
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"barq/internal/agent"
)

var (
	serverAddr       = flag.String("server", "localhost:8080", "Barq controller gRPC address")
	agentID          = flag.String("agent-id", "", "Unique agent identifier")
	hostname         = flag.String("hostname", "", "Agent hostname")
	platform         = flag.String("platform", "", "Agent platform (e.g., linux/amd64)")
	backend          = flag.String("backend", "kubernetes", "Execution backend name")
	capacity         = flag.Int("capacity", 1, "Maximum concurrent workflows")
	labelsStr        = flag.String("labels", "", "Comma-separated key=value labels")
	keepaliveTime    = flag.Duration("keepalive-time", 30*time.Second, "gRPC keepalive interval")
	keepaliveTimeout = flag.Duration("keepalive-timeout", 10*time.Second, "gRPC keepalive timeout")
	logLevel         = flag.String("log-level", "info", "Log level (debug, info, warn, error)")
)

func main() {
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *agentID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "barq-agent: --agent-id is required\n")
			os.Exit(1)
		}
		*agentID = hostname
	}

	if *hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "barq-agent: --hostname is required\n")
			os.Exit(1)
		}
		*hostname = h
	}

	labels := parseLabels(*labelsStr)

	config := agent.ClientConfig{
		ControllerAddr:   *serverAddr,
		AgentID:          *agentID,
		Hostname:         *hostname,
		Platform:         *platform,
		Backend:          *backend,
		Capacity:         *capacity,
		Labels:           labels,
		KeepaliveTime:    *keepaliveTime,
		KeepaliveTimeout: *keepaliveTimeout,
		Logger:           logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := agent.NewClient(config)
	if err != nil {
		logger.Error("failed to create agent client", "error", err)
		os.Exit(1)
	}
	defer client.Close()

	logger.Info("barq-agent started", "agent_id", *agentID, "server", *serverAddr)

	// TODO: Implement the agent run loop that pulls workflows and executes them.
	// The loop would:
	// 1. Call client.Next() to get a workflow
	// 2. Execute each step using the configured backend
	// 3. Report step state via client.Update()
	// 4. Stream logs via client.EnqueueLog()
	// 5. Mark workflow done via client.Done()
	// 6. Repeat

	<-ctx.Done()
	logger.Info("barq-agent shutting down")
}

func parseLabels(s string) map[string]string {
	labels := make(map[string]string)
	if s == "" {
		return labels
	}
	for _, pair := range splitLabels(s) {
		for k, v := range parseLabelPair(pair) {
			labels[k] = v
		}
	}
	return labels
}

func splitLabels(s string) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	result = append(result, s[start:])
	return result
}

func parseLabelPair(s string) map[string]string {
	result := make(map[string]string)
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			key := s[:i]
			value := s[i+1:]
			result[key] = value
			return result
		}
	}
	if s != "" {
		result[s] = "true"
	}
	return result
}
