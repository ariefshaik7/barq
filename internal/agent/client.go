// Package agent implements the Barq agent protocol client and server.
package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"barq/internal/agent/proto"
	"barq/internal/runner"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

var (
	ErrConnectionLost = errors.New("connection to controller lost")
	ErrNotConnected   = errors.New("not connected to controller")
)

const (
	// AgentProtocolVersion is the version of the agent protocol.
	AgentProtocolVersion = "1"

	// Default gRPC keepalive settings.
	defaultKeepaliveTime    = 30 * time.Second
	defaultKeepaliveTimeout = 10 * time.Second

	// Log batching settings.
	maxLogBatchSize   = 1 * 1024 * 1024 // 1 MiB
	maxLogFlushPeriod = 1 * time.Second
)

// ClientConfig configures the agent client.
type ClientConfig struct {
	// ControllerAddr is the address of the Barq controller (e.g., "controller:8080").
	ControllerAddr string
	// AgentID is the unique identifier of this agent.
	AgentID string
	// Hostname is the hostname of the agent machine.
	Hostname string
	// Platform is the platform (e.g., "linux/amd64").
	Platform string
	// Backend is the backend name (e.g., "kubernetes", "docker").
	Backend string
	// Capacity is the maximum number of concurrent workflows.
	Capacity int
	// Labels are custom labels for scheduling.
	Labels map[string]string
	// TLSConfig configures TLS for the connection.
	TLSConfig *tls.Config
	// InsecureSkipVerify skips TLS certificate verification.
	InsecureSkipVerify bool
	// KeepaliveTime is the interval between keepalive pings.
	KeepaliveTime time.Duration
	// KeepaliveTimeout is the timeout for keepalive pings.
	KeepaliveTimeout time.Duration
	// Logger receives the client's diagnostics.
	Logger *slog.Logger
}

// Client is the gRPC client for the Barq agent protocol.
type Client struct {
	config ClientConfig
	logger *slog.Logger

	conn   *grpc.ClientConn
	client proto.BarqAgentClient

	mu         sync.Mutex
	connected  bool
	workflowID string
	cancelFunc context.CancelFunc
	logCh      chan *proto.LogEntry
	logWg      sync.WaitGroup
}

// NewClient creates a new agent client.
func NewClient(config ClientConfig) (*Client, error) {
	if config.ControllerAddr == "" {
		return nil, fmt.Errorf("controller address is required")
	}
	if config.AgentID == "" {
		return nil, fmt.Errorf("agent ID is required")
	}
	if config.Capacity <= 0 {
		config.Capacity = 1
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.KeepaliveTime <= 0 {
		config.KeepaliveTime = defaultKeepaliveTime
	}
	if config.KeepaliveTimeout <= 0 {
		config.KeepaliveTimeout = defaultKeepaliveTimeout
	}

	// Build gRPC dial options.
	dialOpts := []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                config.KeepaliveTime,
			Timeout:             config.KeepaliveTimeout,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  10 * time.Millisecond,
				Multiplier: 1.5,
				MaxDelay:   10 * time.Second,
			},
			MinConnectTimeout: 5 * time.Second,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(16*1024*1024), // 16 MiB
			grpc.MaxCallSendMsgSize(16*1024*1024),
		),
	}

	if config.TLSConfig != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(config.TLSConfig)))
	} else if config.InsecureSkipVerify {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, config.ControllerAddr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial controller: %w", err)
	}

	c := &Client{
		config: config,
		logger: config.Logger,
		conn:   conn,
		client: proto.NewBarqAgentClient(conn),
		logCh:  make(chan *proto.LogEntry, 100),
	}

	// Register the agent.
	if err := c.register(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("register agent: %w", err)
	}

	// Start the log processor.
	c.logWg.Add(1)
	go c.processLogs(context.Background())

	// Start the connection monitor.
	go c.monitorConnection()

	return c, nil
}

// register registers the agent with the controller.
func (c *Client) register(ctx context.Context) error {
	info := &proto.AgentInfo{
		Id:           c.config.AgentID,
		Hostname:     c.config.Hostname,
		Platform:     c.config.Platform,
		Capacity:     int32(c.config.Capacity),
		Backend:      c.config.Backend,
		Version:      AgentProtocolVersion,
		Labels:       c.config.Labels,
		RegisteredAt: time.Now().Unix(),
	}
	resp, err := c.client.Register(ctx, &proto.RegisterRequest{Info: info})
	if err != nil {
		return err
	}
	c.logger.Info("registered with controller", "agent_id", resp.AgentId)
	return nil
}

// monitorConnection watches the gRPC connection state.
func (c *Client) monitorConnection() {
	for {
		state := c.conn.GetState()
		c.mu.Lock()
		wasConnected := c.connected
		c.connected = (state == connectivity.Ready || state == connectivity.Idle)
		if wasConnected && !c.connected {
			c.logger.Warn("connection to controller lost")
		} else if !wasConnected && c.connected {
			c.logger.Info("connection to controller restored")
		}
		c.mu.Unlock()

		if state == connectivity.Shutdown {
			return
		}
		time.Sleep(1 * time.Second)
	}
}

// IsConnected returns whether the client is connected to the controller.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Next requests the next workflow from the controller.
func (c *Client) Next(ctx context.Context, filter map[string]string) (*proto.Workflow, error) {
	req := &proto.NextRequest{Filter: &proto.Filter{Labels: filter}}
	resp, err := c.client.Next(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Workflow, nil
}

// Init signals that the workflow has been initialized.
func (c *Client) Init(ctx context.Context, workflowID string) error {
	req := &proto.InitRequest{
		WorkflowId: workflowID,
		State:      &proto.WorkflowState{Started: time.Now().Unix()},
	}
	_, err := c.client.Init(ctx, req)
	return err
}

// Wait blocks until the workflow is canceled.
func (c *Client) Wait(ctx context.Context, workflowID string) (bool, error) {
	req := &proto.WaitRequest{WorkflowId: workflowID}
	resp, err := c.client.Wait(ctx, req)
	if err != nil {
		return false, err
	}
	return resp.Canceled, nil
}

// Done signals that the workflow has finished.
func (c *Client) Done(ctx context.Context, workflowID string, state runner.State) error {
	workflowState := &proto.WorkflowState{
		Started:  time.Now().Unix(),
		Finished: time.Now().Unix(),
		Error:    state.Err.Error(),
		Canceled: errors.Is(state.Err, runner.ErrCanceled),
	}
	req := &proto.DoneRequest{
		WorkflowId: workflowID,
		State:      workflowState,
	}
	_, err := c.client.Done(ctx, req)
	return err
}

// Extend extends the workflow deadline.
func (c *Client) Extend(ctx context.Context, workflowID string) error {
	req := &proto.ExtendRequest{WorkflowId: workflowID}
	_, err := c.client.Extend(ctx, req)
	return err
}

// Update updates the state of a step.
func (c *Client) Update(ctx context.Context, workflowID, stepID string, state runner.State) error {
	stepState := &proto.StepState{
		StepId:   stepID,
		Started:  state.Started.Unix(),
		Finished: state.Finished.Unix(),
		Exited:   state.Exited,
		ExitCode: int32(state.ExitCode),
		Error:    "",
		Canceled: errors.Is(state.Err, runner.ErrCanceled),
		Skipped:  false,
	}
	if state.Err != nil && !errors.Is(state.Err, runner.ErrCanceled) {
		stepState.Error = state.Err.Error()
	}
	req := &proto.UpdateRequest{
		WorkflowId: workflowID,
		State:      stepState,
	}
	_, err := c.client.Update(ctx, req)
	return err
}

// EnqueueLog queues a log entry for batched sending.
func (c *Client) EnqueueLog(entry *proto.LogEntry) {
	select {
	case c.logCh <- entry:
	default:
		// Buffer full, drop the oldest entry and add the new one.
		select {
		case <-c.logCh:
		default:
		}
		c.logCh <- entry
	}
}

// processLogs batches and sends log entries.
func (c *Client) processLogs(ctx context.Context) {
	defer c.logWg.Done()

	var entries []*proto.LogEntry
	var bytes int
	ticker := time.NewTicker(maxLogFlushPeriod)
	defer ticker.Stop()

	send := func() error {
		if len(entries) == 0 {
			return nil
		}
		req := &proto.LogRequest{LogEntries: entries}
		if _, err := c.client.Log(ctx, req); err != nil {
			c.logger.Error("failed to send logs", "error", err)
			return err
		}
		entries = entries[:0]
		bytes = 0
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			_ = send()
			return
		case entry, ok := <-c.logCh:
			if !ok {
				_ = send()
				return
			}
			entries = append(entries, entry)
			bytes += len(entry.Data)
			if bytes >= maxLogBatchSize {
				_ = send()
			}
		case <-ticker.C:
			_ = send()
		}
	}
}

// ReportHealth reports the agent's health to the controller.
func (c *Client) ReportHealth(ctx context.Context, status string, runningWorkflows int) error {
	req := &proto.HealthRequest{
		AgentId:          c.config.AgentID,
		Status:           status,
		RunningWorkflows: int32(runningWorkflows),
	}
	_, err := c.client.Health(ctx, req)
	return err
}

// Close closes the client connection.
func (c *Client) Close() error {
	close(c.logCh)
	c.logWg.Wait()
	c.conn.Close()
	return nil
}
