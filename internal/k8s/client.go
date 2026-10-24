// Package k8s provides the Kubernetes execution backend.
//
// The client talks to the in-cluster API server directly over the REST
// API using only the Go standard library. It installs no CRDs, so it adds
// no etcd bloat beyond the Lease used for leader election.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// K8sMicroTimeLayout is the timestamp layout used for Lease fields.
//
// Go's time package does not provide time.RFC3339Micro, so the layout is
// declared explicitly. Kubernetes accepts RFC 3339 timestamps, and the
// microsecond form matches the API server output.
const K8sMicroTimeLayout = "2006-01-02T15:04:05.000000Z"

// serviceAccountPath is the mount path of the projected service account.
const serviceAccountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

// Worker is the description of a worker Pod that Barq wants to run.
type Worker struct {
	// Name is the deterministic Pod name.
	Name string
	// RunID identifies the run.
	RunID string
	// StepID identifies the compiled step.
	StepID string
	// Wave is the execution wave of the step.
	Wave int
	// Attempt is the zero based attempt number.
	Attempt int
	// Image is the container image.
	Image string
	// Commands are the shell commands to execute.
	Commands []string
	// Env is the environment for the container.
	Env map[string]string
	// WorkingDir is the container working directory.
	WorkingDir string
	// TimeoutSeconds bounds the step runtime.
	TimeoutSeconds int
	// WorkspaceDownloads are pre-signed URLs of upstream workspaces.
	WorkspaceDownloads []string
	// WorkspaceUpload is the pre-signed URL the worker uploads to.
	WorkspaceUpload string
	// NoWorkspace disables workspace capture for this step.
	NoWorkspace bool
	// Services are sidecar containers that run alongside the worker.
	Services []Sidecar
	// Requests and Limits override the default worker resources.
	Requests map[string]string
	Limits   map[string]string
	// ServiceAccount runs the worker as a specific service account.
	ServiceAccount string
	// NodeSelector constrains which nodes may run the worker.
	NodeSelector map[string]string
	// Tolerations allow the worker to run on tainted nodes.
	Tolerations []string
}

// Pod is the subset of a Pod that Barq consumes.
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   PodStatus  `json:"status"`
}

// ObjectMeta is the Kubernetes object metadata Barq needs.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	CreationTimestamp string            `json:"creationTimestamp,omitempty"`
}

// PodStatus is the subset of Pod status Barq consumes.
type PodStatus struct {
	Phase             string          `json:"phase"`
	Reason            string          `json:"reason,omitempty"`
	Message           string          `json:"message,omitempty"`
	ContainerStatuses []ContainerStat `json:"containerStatuses,omitempty"`
	StartTime         string          `json:"startTime,omitempty"`
}

// ContainerStat is the subset of container status Barq consumes.
type ContainerStat struct {
	Name  string         `json:"name"`
	State ContainerState `json:"state"`
}

// ContainerState holds the running, waiting or terminated state.
type ContainerState struct {
	Waiting    *WaitingState    `json:"waiting,omitempty"`
	Running    *RunningState    `json:"running,omitempty"`
	Terminated *TerminatedState `json:"terminated,omitempty"`
}

// WaitingState describes a container waiting to start.
type WaitingState struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// RunningState describes a running container.
type RunningState struct {
	StartedAt string `json:"startedAt,omitempty"`
}

// TerminatedState describes a finished container.
type TerminatedState struct {
	ExitCode   int    `json:"exitCode"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// ExitCode returns the container exit code, if the container terminated.
func (p Pod) ExitCode() (int, bool) {
	for _, status := range p.Status.ContainerStatuses {
		if status.State.Terminated != nil {
			return status.State.Terminated.ExitCode, true
		}
	}
	return 0, false
}

// Terminated reports whether the pod has finished.
func (p Pod) Terminated() bool {
	switch p.Status.Phase {
	case "Succeeded", "Failed":
		return true
	}
	code, ok := p.ExitCode()
	return ok && code != 0
}

// Lease is a coordination.k8s.io/v1 Lease.
type Lease struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       LeaseSpec  `json:"spec"`
}

// LeaseSpec is the Lease spec.
type LeaseSpec struct {
	HolderIdentity       string `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds *int32 `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          string `json:"acquireTime,omitempty"`
	RenewTime            string `json:"renewTime,omitempty"`
	LeaseTransitions     int32  `json:"leaseTransitions,omitempty"`
}

// Backend is the execution backend contract.
//
// The interface combines the low level primitives with the step
// lifecycle that the runner drives. The lifecycle methods are the
// ones the controller uses; the primitives are exposed for the
// operations that need them directly, such as listing the workers of
// a run during a leader failover.
type Backend interface {
	// Spawn creates a worker Pod.
	Spawn(ctx context.Context, worker Worker) error
	// GetPod retrieves a Pod.
	GetPod(ctx context.Context, name string) (Pod, error)
	// ListRunPods lists the Pods belonging to a run.
	ListRunPods(ctx context.Context, runID string) ([]Pod, error)
	// DeletePod removes a Pod.
	DeletePod(ctx context.Context, name string) error
	// PodLogs opens a log stream for a Pod.
	PodLogs(ctx context.Context, name string, since time.Time) (io.ReadCloser, error)
	// GetLease retrieves a Lease.
	GetLease(ctx context.Context, name string) (Lease, error)
	// CreateLease creates a Lease.
	CreateLease(ctx context.Context, lease Lease) error
	// UpdateLease updates a Lease using optimistic concurrency.
	UpdateLease(ctx context.Context, lease Lease) error
	// Name returns the backend name.
	Name() string

	// The step lifecycle below is what the runner drives.

	// SetupWorkflow prepares the resources a workflow needs.
	SetupWorkflow(ctx context.Context, workflowID string) error
	// StartStep starts a worker for the step.
	StartStep(ctx context.Context, step Step) error
	// TailStep opens the log stream of a running worker.
	TailStep(ctx context.Context, step Step) (io.ReadCloser, error)
	// WaitStep waits for the worker to finish and reports its state.
	WaitStep(ctx context.Context, step Step) (StepState, error)
	// DestroyStep stops and removes the worker.
	DestroyStep(ctx context.Context, step Step) error
	// DestroyWorkflow removes the resources of a workflow.
	DestroyWorkflow(ctx context.Context, workflowID string) error
}

// APIError is a non 2xx response from the Kubernetes API server.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, strings.TrimSpace(e.Body))
}

// IsNotFound reports whether an error is a 404 from the API server.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsConflict reports whether an error is a 409 from the API server.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// IsAlreadyExists reports whether an error is a 409 already-exists.
func IsAlreadyExists(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Body), "alreadyexists")
}

// Client is the in-cluster Kubernetes REST client.
type Client struct {
	base       *url.URL
	tokenFile  string
	namespace  string
	httpClient *http.Client
}

// NewInCluster creates a client from the mounted service account.
func NewInCluster(namespace string) (*Client, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running inside Kubernetes: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set")
	}
	if namespace == "" {
		raw, err := os.ReadFile(serviceAccountPath + "/namespace")
		if err != nil {
			return nil, fmt.Errorf("read service account namespace: %w", err)
		}
		namespace = strings.TrimSpace(string(raw))
	}

	pool := x509.NewCertPool()
	ca, err := os.ReadFile(serviceAccountPath + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("service account CA is not valid PEM")
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
		MaxIdleConns:        32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	return &Client{
		base:       &url.URL{Scheme: "https", Host: host + ":" + port},
		tokenFile:  serviceAccountPath + "/token",
		namespace:  namespace,
		httpClient: &http.Client{Transport: transport},
	}, nil
}

// Name implements Backend.
func (c *Client) Name() string { return "kubernetes" }

// token reads the service account token.
//
// The token is read on every request because Kubernetes projects a
// rotating token into the filesystem. Caching it for the lifetime of the
// process would eventually produce authentication failures.
func (c *Client) token() (string, error) {
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read service account token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("service account token is empty")
	}
	return token, nil
}

// request performs an authenticated API call.
func (c *Client) request(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	target := *c.base
	target.Path = path
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}

	var reader io.Reader
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = encoded
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return err
	}
	token, err := c.token()
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return &APIError{
			StatusCode: response.StatusCode,
			Method:     method,
			Path:       path,
			Body:       string(detail),
		}
	}
	if out != nil {
		return json.NewDecoder(response.Body).Decode(out)
	}
	return nil
}

// Spawn implements Backend.
func (c *Client) Spawn(ctx context.Context, worker Worker) error {
	environment := make([]EnvVar, 0, len(worker.Env)+6)
	for key, value := range worker.Env {
		environment = append(environment, EnvVar{Name: key, Value: value})
	}
	environment = append(environment,
		EnvVar{Name: "BARQ_RUN_ID", Value: worker.RunID},
		EnvVar{Name: "BARQ_STEP_ID", Value: worker.StepID},
		EnvVar{Name: "BARQ_WAVE", Value: strconv.Itoa(worker.Wave)},
		EnvVar{Name: "BARQ_ATTEMPT", Value: strconv.Itoa(worker.Attempt)},
	)
	if worker.NoWorkspace {
		environment = append(environment, EnvVar{Name: "BARQ_NO_WORKSPACE", Value: "1"})
	}
	if len(worker.WorkspaceDownloads) > 0 {
		environment = append(environment, EnvVar{Name: "BARQ_WORKSPACE_DOWNLOADS", Value: strings.Join(worker.WorkspaceDownloads, "\n")})
	}
	if worker.WorkspaceUpload != "" {
		environment = append(environment, EnvVar{Name: "BARQ_WORKSPACE_UPLOAD", Value: worker.WorkspaceUpload})
	}

	command := []string{"/bin/sh", "-c"}
	args := []string{WorkerScript, "barq-worker"}
	args = append(args, worker.Commands...)

	container := Container{
		Name:       "worker",
		Image:      worker.Image,
		Command:    command,
		Args:       args,
		Env:        environment,
		WorkingDir: workingDir(worker.WorkingDir),
		Resources: Resources{
			Requests: resourceList(worker.Requests, defaultRequests),
			Limits:   resourceList(worker.Limits, defaultLimits),
		},
		VolumeMounts: []VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
	}
	if worker.TimeoutSeconds > 0 {
		container.ActiveDeadlineSeconds = int64Ptr(int64(worker.TimeoutSeconds))
	}

	// The sidecar containers are appended after the worker, so that the
	// worker is always the first container and its name and index are
	// stable.
	containers := []Container{container}
	for _, sidecar := range worker.Services {
		sidecarEnvironment := make([]EnvVar, 0, len(sidecar.Env))
		for key, value := range sidecar.Env {
			sidecarEnvironment = append(sidecarEnvironment, EnvVar{Name: key, Value: value})
		}
		sidecarContainer := Container{
			Name:         sanitizeLabel(sidecar.ID),
			Image:        sidecar.Image,
			Env:          sidecarEnvironment,
			Resources:    Resources{Requests: ResourceList{"cpu": "100m", "memory": "64Mi"}},
			VolumeMounts: []VolumeMount{{Name: "workspace", MountPath: "/workspace"}},
		}
		if len(sidecar.Commands) > 0 {
			sidecarContainer.Command = []string{"/bin/sh", "-c"}
			sidecarContainer.Args = []string{scripts(sidecar.Commands)}
		}
		containers = append(containers, sidecarContainer)
	}

	spec := PodSpecBody{
		RestartPolicy:                 "Never",
		AutomountServiceAccountToken:  boolPtr(false),
		TerminationGracePeriodSeconds: int64Ptr(20),
		Containers:                    containers,
		Volumes: []Volume{{
			Name: "workspace",
			EmptyDir: &EmptyDir{
				SizeLimit: "10Gi",
			},
		}},
	}
	if worker.ServiceAccount != "" {
		spec.ServiceAccountName = worker.ServiceAccount
	}
	if len(worker.NodeSelector) > 0 {
		spec.NodeSelector = worker.NodeSelector
	}
	for _, toleration := range worker.Tolerations {
		spec.Tolerations = append(spec.Tolerations, Toleration{Key: toleration})
	}

	pod := PodSpec{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata: ObjectMeta{
			Name:      worker.Name,
			Namespace: c.namespace,
			Labels: map[string]string{
				"barq.io/run-id":               sanitizeLabel(worker.RunID),
				"barq.io/step-id":              sanitizeLabel(worker.StepID),
				"barq.io/wave":                 strconv.Itoa(worker.Wave),
				"barq.io/attempt":              strconv.Itoa(worker.Attempt),
				"app.kubernetes.io/managed-by": "barq",
			},
		},
		Spec: spec,
	}

	path := "/api/v1/namespaces/" + url.PathEscape(c.namespace) + "/pods"
	return c.request(ctx, http.MethodPost, path, nil, pod, nil)
}

// GetPod implements Backend.
func (c *Client) GetPod(ctx context.Context, name string) (Pod, error) {
	var pod Pod
	path := "/api/v1/namespaces/" + url.PathEscape(c.namespace) + "/pods/" + url.PathEscape(name)
	if err := c.request(ctx, http.MethodGet, path, nil, nil, &pod); err != nil {
		return Pod{}, err
	}
	return pod, nil
}

// ListRunPods implements Backend.
func (c *Client) ListRunPods(ctx context.Context, runID string) ([]Pod, error) {
	var list struct {
		Items []Pod `json:"items"`
	}
	query := url.Values{}
	query.Set("labelSelector", "barq.io/run-id="+sanitizeLabel(runID))
	path := "/api/v1/namespaces/" + url.PathEscape(c.namespace) + "/pods"
	if err := c.request(ctx, http.MethodGet, path, query, nil, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// DeletePod implements Backend.
func (c *Client) DeletePod(ctx context.Context, name string) error {
	path := "/api/v1/namespaces/" + url.PathEscape(c.namespace) + "/pods/" + url.PathEscape(name)
	return c.request(ctx, http.MethodDelete, path, nil, nil, nil)
}

// PodLogs implements Backend.
func (c *Client) PodLogs(ctx context.Context, name string, since time.Time) (io.ReadCloser, error) {
	query := url.Values{}
	query.Set("follow", "true")
	query.Set("container", "worker")
	query.Set("timestamps", "true")
	if !since.IsZero() {
		query.Set("sinceTime", since.UTC().Format(time.RFC3339Nano))
	}
	target := *c.base
	target.Path = "/api/v1/namespaces/" + url.PathEscape(c.namespace) + "/pods/" + url.PathEscape(name) + "/log"
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	token, err := c.token()
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		defer response.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return nil, &APIError{
			StatusCode: response.StatusCode,
			Method:     http.MethodGet,
			Path:       target.Path,
			Body:       string(detail),
		}
	}
	return response.Body, nil
}

// GetLease implements Backend.
func (c *Client) GetLease(ctx context.Context, name string) (Lease, error) {
	var lease Lease
	path := "/apis/coordination.k8s.io/v1/namespaces/" + url.PathEscape(c.namespace) + "/leases/" + url.PathEscape(name)
	if err := c.request(ctx, http.MethodGet, path, nil, nil, &lease); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// CreateLease implements Backend.
func (c *Client) CreateLease(ctx context.Context, lease Lease) error {
	lease.APIVersion = "coordination.k8s.io/v1"
	lease.Kind = "Lease"
	lease.Metadata.Namespace = c.namespace
	path := "/apis/coordination.k8s.io/v1/namespaces/" + url.PathEscape(c.namespace) + "/leases"
	return c.request(ctx, http.MethodPost, path, nil, lease, nil)
}

// UpdateLease implements Backend.
//
// The caller must carry the resourceVersion that was read from the API
// server. The API server rejects the update with a 409 conflict if the
// version is stale, which is what provides the compare-and-swap
// semantics that prevent split brain.
func (c *Client) UpdateLease(ctx context.Context, lease Lease) error {
	lease.APIVersion = "coordination.k8s.io/v1"
	lease.Kind = "Lease"
	lease.Metadata.Namespace = c.namespace
	path := "/apis/coordination.k8s.io/v1/namespaces/" + url.PathEscape(c.namespace) + "/leases/" + url.PathEscape(lease.Metadata.Name)
	return c.request(ctx, http.MethodPut, path, nil, lease, nil)
}

// PodSpec is the request body used to create a Pod.
type PodSpec struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   ObjectMeta  `json:"metadata"`
	Spec       PodSpecBody `json:"spec"`
}

// PodSpecBody is the Pod spec.
type PodSpecBody struct {
	RestartPolicy                 string            `json:"restartPolicy"`
	ServiceAccountName            string            `json:"serviceAccountName,omitempty"`
	NodeSelector                  map[string]string `json:"nodeSelector,omitempty"`
	Tolerations                   []Toleration      `json:"tolerations,omitempty"`
	AutomountServiceAccountToken  *bool             `json:"automountServiceAccountToken,omitempty"`
	TerminationGracePeriodSeconds *int64            `json:"terminationGracePeriodSeconds,omitempty"`
	ActiveDeadlineSeconds         *int64            `json:"activeDeadlineSeconds,omitempty"`
	Containers                    []Container       `json:"containers"`
	Volumes                       []Volume          `json:"volumes,omitempty"`
	SecurityContext               *PodSecurity      `json:"securityContext,omitempty"`
}

// Toleration allows a Pod to run on a tainted node.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

// PodSecurity holds pod level security settings.
type PodSecurity struct {
	RunAsNonRoot *bool  `json:"runAsNonRoot,omitempty"`
	RunAsUser    *int64 `json:"runAsUser,omitempty"`
	FSGroup      *int64 `json:"fsGroup,omitempty"`
}

// Container describes a worker container.
type Container struct {
	Name                  string        `json:"name"`
	Image                 string        `json:"image"`
	ImagePullPolicy       string        `json:"imagePullPolicy,omitempty"`
	Command               []string      `json:"command,omitempty"`
	Args                  []string      `json:"args,omitempty"`
	Env                   []EnvVar      `json:"env,omitempty"`
	WorkingDir            string        `json:"workingDir,omitempty"`
	Resources             Resources     `json:"resources,omitempty"`
	ActiveDeadlineSeconds *int64        `json:"activeDeadlineSeconds,omitempty"`
	VolumeMounts          []VolumeMount `json:"volumeMounts,omitempty"`
	SecurityContext       *ContainerSec `json:"securityContext,omitempty"`
}

// ContainerSec holds container level security settings.
type ContainerSec struct {
	AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation,omitempty"`
	ReadOnlyRootFilesystem   *bool `json:"readOnlyRootFilesystem,omitempty"`
	Capabilities             *Caps `json:"capabilities,omitempty"`
}

// Caps holds container capabilities.
type Caps struct {
	Drop []string `json:"drop,omitempty"`
}

// Resources describes container resource requests and limits.
type Resources struct {
	Requests ResourceList `json:"requests,omitempty"`
	Limits   ResourceList `json:"limits,omitempty"`
}

// ResourceList is a map of resource name to quantity.
type ResourceList map[string]string

// EnvVar is a container environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// VolumeMount mounts a volume into a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath,omitempty"`
}

// Volume is a Pod volume.
type Volume struct {
	Name     string    `json:"name"`
	EmptyDir *EmptyDir `json:"emptyDir,omitempty"`
}

// EmptyDir configures an ephemeral volume.
type EmptyDir struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}

func workingDir(value string) string {
	if value == "" {
		return "/workspace"
	}
	return value
}

// scripts renders a list of commands as a single shell script.
//
// The commands are joined with a newline and each one is run with the
// shell's error handling, so that a failure stops the script.
func scripts(commands []string) string {
	var builder strings.Builder
	builder.WriteString("set -e\n")
	for _, command := range commands {
		builder.WriteString(command)
		builder.WriteString("\n")
	}
	return builder.String()
}

// defaultRequests and defaultLimits are the worker resource defaults.
// They are deliberately small so that a fan-out of a hundred shards
// does not saturate a cluster, and generous enough that a typical
// compile and test cycle fits inside them.
var (
	defaultRequests = map[string]string{"cpu": "100m", "memory": "128Mi"}
	defaultLimits   = map[string]string{"cpu": "2", "memory": "2Gi"}
)

// resourceList merges an override with the defaults.
func resourceList(override, defaults map[string]string) ResourceList {
	result := ResourceList{}
	for name, value := range defaults {
		result[name] = value
	}
	for name, value := range override {
		result[name] = value
	}
	return result
}

func boolPtr(value bool) *bool { return &value }

func int64Ptr(value int64) *int64 { return &value }

// sanitizeLabel converts an arbitrary string into a valid label value.
func sanitizeLabel(value string) string {
	if len(value) > 63 {
		value = value[:63]
	}
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-', char == '.':
			builder.WriteRune(char)
		default:
			builder.WriteByte('-')
		}
	}
	result := strings.Trim(builder.String(), "-.")
	if result == "" {
		return "unknown"
	}
	return result
}
