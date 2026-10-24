// Package agent implements the Barq agent protocol client and server.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"barq/internal/agent/proto"
	"barq/internal/controller"
	"barq/internal/queue"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ServerConfig configures the agent gRPC server.
type ServerConfig struct {
	// Engine is the Barq controller engine.
	Engine *controller.Engine
	// Queue is the task queue.
	Queue *queue.Queue
	// Logger receives the server's diagnostics.
	Logger *slog.Logger
	// MaxConcurrentStreams limits concurrent gRPC streams.
	MaxConcurrentStreams uint32
}

// Server is the gRPC server for the Barq agent protocol.
type Server struct {
	proto.UnimplementedBarqAgentServer
	proto.UnimplementedBarqControllerServer

	config ServerConfig
	logger *slog.Logger

	mu     sync.Mutex
	agents map[string]*AgentInfo
}

// AgentInfo holds information about a registered agent.
type AgentInfo struct {
	info       *proto.AgentInfo
	connected  bool
	lastSeen   time.Time
	workflowID string
	cancelFunc context.CancelFunc
}

// protoInfo returns the underlying proto.AgentInfo.
func (a *AgentInfo) protoInfo() *proto.AgentInfo {
	return a.info
}

// NewServer creates a new agent gRPC server.
func NewServer(config ServerConfig) *Server {
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.MaxConcurrentStreams == 0 {
		config.MaxConcurrentStreams = 100
	}
	return &Server{
		config: config,
		logger: config.Logger,
		agents: make(map[string]*AgentInfo),
	}
}

// RegisterAgent implements the BarqAgent.Register RPC.
func (s *Server) RegisterAgent(ctx context.Context, req *proto.RegisterRequest) (*proto.RegisterResponse, error) {
	if req.Info == nil {
		return nil, status.Error(codes.InvalidArgument, "agent info is required")
	}
	if req.Info.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "agent ID is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	agent := &AgentInfo{
		info:      req.Info,
		connected: true,
		lastSeen:  time.Now(),
	}
	s.agents[req.Info.Id] = agent

	s.config.Logger.Info("agent registered", "agent_id", req.Info.Id, "hostname", req.Info.Hostname)

	// Generate a simple token (in production, use JWT).
	token := fmt.Sprintf("agent-%s-%d", req.Info.Id, time.Now().Unix())

	return &proto.RegisterResponse{
		AgentId: req.Info.Id,
		Token:   token,
	}, nil
}

// Next implements the BarqAgent.Next RPC.
func (s *Server) Next(ctx context.Context, req *proto.NextRequest) (*proto.NextResponse, error) {
	// Extract agent ID from context (set by auth interceptor in production).
	agentID := getAgentID(ctx)
	if agentID == "" {
		return nil, status.Error(codes.Unauthenticated, "agent ID not found in context")
	}

	s.mu.Lock()
	agent, ok := s.agents[agentID]
	if !ok {
		s.mu.Unlock()
		return nil, status.Error(codes.NotFound, "agent not registered")
	}
	agent.lastSeen = time.Now()
	s.mu.Unlock()

	// Build the label filter from the request.
	var labelFilter map[string]string
	if req.Filter != nil {
		labelFilter = req.Filter.Labels
	}

	// Poll for a task from the queue.
	task, release, err := s.config.Queue.Poll(ctx, agentID, func(t *queue.Task) (bool, int) {
		// Check if the task matches the agent's labels.
		if labelFilter != nil {
			for k, v := range labelFilter {
				if t.Labels[k] != v {
					return false, 0
				}
			}
		}
		return true, 1
	})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return &proto.NextResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "queue poll failed: %v", err)
	}
	if task == nil {
		return &proto.NextResponse{}, nil
	}

	// Get the run state from the engine.
	run, ok := s.config.Engine.GetRun(ctx, task.ID)
	if !ok {
		release(queue.StatusFailure, fmt.Errorf("run not found"))
		return nil, status.Error(codes.NotFound, "run not found")
	}

	// Serialize the pipeline spec for the agent.
	payload, err := serializePipeline(run.Pipeline)
	if err != nil {
		release(queue.StatusFailure, err)
		return nil, status.Errorf(codes.Internal, "serialize pipeline: %v", err)
	}

	// Update agent state.
	s.mu.Lock()
	agent.workflowID = task.ID
	s.mu.Unlock()

	return &proto.NextResponse{
		Workflow: &proto.Workflow{
			Id:      task.ID,
			Timeout: int64(task.Deadline.Sub(time.Now()).Milliseconds()),
			Payload: payload,
		},
	}, nil
}

// Init implements the BarqAgent.Init RPC.
func (s *Server) Init(ctx context.Context, req *proto.InitRequest) (*proto.Empty, error) {
	if _, ok := s.config.Engine.GetRun(ctx, req.WorkflowId); !ok {
		return nil, status.Error(codes.NotFound, "run not found")
	}
	return &proto.Empty{}, nil
}

// Wait implements the BarqAgent.Wait RPC.
func (s *Server) Wait(ctx context.Context, req *proto.WaitRequest) (*proto.WaitResponse, error) {
	agentID := getAgentID(ctx)
	if agentID == "" {
		return nil, status.Error(codes.Unauthenticated, "agent ID not found")
	}

	// Wait for the run to be canceled or completed.
	// This is a long-polling RPC that blocks until the controller cancels the run.
	done := make(chan struct{})
	var canceled bool

	s.config.Engine.GetRun(ctx, req.WorkflowId) // Ensure run exists.

	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run, ok := s.config.Engine.GetRun(ctx, req.WorkflowId)
				if !ok || run.IsTerminal() {
					canceled = run != nil && run.Status == controller.RunCanceled
					close(done)
					return
				}
			}
		}
	}()

	select {
	case <-ctx.Done():
		return &proto.WaitResponse{Canceled: false}, ctx.Err()
	case <-done:
		return &proto.WaitResponse{Canceled: canceled}, nil
	}
}

// Done implements the BarqAgent.Done RPC.
func (s *Server) Done(ctx context.Context, req *proto.DoneRequest) (*proto.Empty, error) {
	agentID := getAgentID(ctx)
	if agentID == "" {
		return nil, status.Error(codes.Unauthenticated, "agent ID not found")
	}

	s.mu.Lock()
	agent, ok := s.agents[agentID]
	if ok {
		agent.workflowID = ""
	}
	s.mu.Unlock()

	// The engine handles the final state via the normal reconciliation loop.
	return &proto.Empty{}, nil
}

// Extend implements the BarqAgent.Extend RPC.
func (s *Server) Extend(ctx context.Context, req *proto.ExtendRequest) (*proto.Empty, error) {
	agentID := getAgentID(ctx)
	if agentID == "" {
		return nil, status.Error(codes.Unauthenticated, "agent ID not found")
	}

	// Extend the task deadline in the queue.
	if err := s.config.Queue.Extend(ctx, req.WorkflowId); err != nil {
		return nil, status.Errorf(codes.Internal, "extend failed: %v", err)
	}
	return &proto.Empty{}, nil
}

// Update implements the BarqAgent.Update RPC.
func (s *Server) Update(ctx context.Context, req *proto.UpdateRequest) (*proto.Empty, error) {
	agentID := getAgentID(ctx)
	if agentID == "" {
		return nil, status.Error(codes.Unauthenticated, "agent ID not found")
	}

	// The controller's engine will receive step state updates via the normal
	// reconciliation loop (it watches the pods). This RPC is for external
	// agents that don't run as Kubernetes pods.
	// For now, we just acknowledge the update.
	return &proto.Empty{}, nil
}

// Log implements the BarqAgent.Log RPC.
func (s *Server) Log(ctx context.Context, req *proto.LogRequest) (*proto.Empty, error) {
	agentID := getAgentID(ctx)
	if agentID == "" {
		return nil, status.Error(codes.Unauthenticated, "agent ID not found")
	}

	s.mu.Lock()
	agent, ok := s.agents[agentID]
	s.mu.Unlock()
	if !ok || agent.workflowID == "" {
		return nil, status.Error(codes.FailedPrecondition, "agent has no active workflow")
	}

	// Forward log entries to the tiered storage.
	for _, entry := range req.LogEntries {
		// Convert proto.LogEntry to log line.
		line := string(entry.Data)
		s.config.Engine.AppendLog(ctx, agent.workflowID, line)
	}

	return &proto.Empty{}, nil
}

// Health implements the BarqAgent.Health RPC.
func (s *Server) Health(ctx context.Context, req *proto.HealthRequest) (*proto.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, ok := s.agents[req.AgentId]
	if !ok {
		return nil, status.Error(codes.NotFound, "agent not registered")
	}
	agent.lastSeen = time.Now()
	agent.info.Status = req.Status

	return &proto.Empty{}, nil
}

// ListAgents implements the BarqController.ListAgents RPC.
func (s *Server) ListAgents(ctx context.Context, req *proto.Empty) (*proto.ListAgentsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agents := make([]*proto.AgentInfo, 0, len(s.agents))
	for _, a := range s.agents {
		agents = append(agents, a.protoInfo())
	}
	return &proto.ListAgentsResponse{Agents: agents}, nil
}

// GetAgent implements the BarqController.GetAgent RPC.
func (s *Server) GetAgent(ctx context.Context, req *proto.GetAgentRequest) (*proto.AgentInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, ok := s.agents[req.AgentId]
	if !ok {
		return nil, status.Error(codes.NotFound, "agent not found")
	}
	return agent.protoInfo(), nil
}

// CancelWorkflow implements the BarqController.CancelWorkflow RPC.
func (s *Server) CancelWorkflow(ctx context.Context, req *proto.CancelWorkflowRequest) (*proto.Empty, error) {
	if err := s.config.Engine.StopRun(ctx, req.WorkflowId); err != nil {
		return nil, status.Errorf(codes.Internal, "cancel workflow: %v", err)
	}
	return &proto.Empty{}, nil
}

// RegisterGRPC registers the agent server with a gRPC server.
func (s *Server) RegisterGRPC(grpcServer *grpc.Server) {
	proto.RegisterBarqAgentServer(grpcServer, s)
	proto.RegisterBarqControllerServer(grpcServer, s)
}

// getAgentID extracts the agent ID from the context.
// In production, this would be set by an auth interceptor.
func getAgentID(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("agent-id"); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

// serializePipeline serializes a pipeline spec to JSON.
func serializePipeline(spec interface{}) ([]byte, error) {
	// This is a placeholder. In reality, you'd use the YAML/JSON marshaling
	// from the yaml package. For now, return empty to satisfy the interface.
	return []byte("{}"), nil
}
