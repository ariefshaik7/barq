// Package forge abstracts the source control providers that Barq
// integrates with.
//
// The interface is deliberately small. Barq needs to do four things
// with a forge: validate a webhook, list the branches, fetch a
// pipeline definition from a commit, and publish a commit status. A
// provider that supports more may implement the optional interfaces.
//
// The contract is documented on the interface. It is the API.
package forge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Errors returned by the forge layer.
var (
	// ErrIgnoreEvent means the webhook is valid but not interesting.
	// The receiver acknowledges it and moves on.
	ErrIgnoreEvent = errors.New("the event is not handled")
	// ErrNotImplemented means the provider does not support the
	// operation.
	ErrNotImplemented = errors.New("the operation is not implemented")
	// ErrNotFound means the resource does not exist.
	ErrNotFound = errors.New("the resource does not exist")
)

// Event is the kind of webhook that arrived.
type Event string

const (
	// EventPush is a branch or tag push.
	EventPush Event = "push"
	// EventPullRequest is a pull request event.
	EventPullRequest Event = "pull_request"
	// EventTag is a tag push.
	EventTag Event = "tag"
	// EventRelease is a release publication.
	EventRelease Event = "release"
	// EventManual is a manually triggered run.
	EventManual Event = "manual"
)

// HookRequest is a validated webhook request.
type HookRequest struct {
	// Event is the kind of event.
	Event Event
	// Repository is the owner and name, for example "acme/widgets".
	Repository string
	// Branch is the branch that was pushed.
	Branch string
	// Commit is the immutable commit that was pushed.
	Commit string
	// PullRequest is the pull request number, or zero.
	PullRequest int
	// Sender is the account that triggered the event.
	Sender string
	// Payload is the raw body.
	Payload []byte
}

// Repository identifies a repository.
type Repository struct {
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	FullName   string `json:"full_name"`
	CloneURL   string `json:"clone_url"`
	DefaultRef string `json:"default_branch"`
	Private    bool   `json:"private"`
}

// Branch is a branch in a repository.
type Branch struct {
	Name string `json:"name"`
	Head string `json:"head"`
}

// Status is a commit status.
type Status struct {
	// State is success, failure, error, pending or in_progress.
	State string `json:"state"`
	// Target is the URL a user should visit to see the result.
	Target string `json:"target"`
	// Description is a short explanation.
	Description string `json:"description"`
	// Context is the name of the check.
	Context string `json:"context"`
}

// Forge is the interface to a source control provider.
//
// Implementations must be safe for concurrent use. Every method takes
// a context so that it can be cancelled. An implementation must not
// maintain per-user state; the user context is passed in the request.
type Forge interface {
	// Name returns the provider name, for example "github".
	Name() string
	// URL returns the provider's base URL.
	URL() string
	// Hook parses and validates an incoming webhook.
	//
	// It returns ErrIgnoreEvent when the payload is valid but does not
	// describe an event that should start a pipeline. The caller
	// acknowledges those with a success response.
	Hook(ctx context.Context, request *http.Request) (*HookRequest, error)
	// Branches lists the branches of a repository.
	Branches(ctx context.Context, token, repository string) ([]Branch, error)
	// Pipeline fetches the pipeline definition from a commit.
	Pipeline(ctx context.Context, token, repository, path, commit string) ([]byte, error)
	// Status publishes a commit status.
	Status(ctx context.Context, token, repository, commit string, status Status) error
}

// Refresher is an optional interface for providers whose credentials
// expire and must be refreshed.
type Refresher interface {
	// Refresh renews the stored credentials of a user.
	Refresh(ctx context.Context, userID string) (bool, error)
}

// Config describes a forge connection.
type Config struct {
	// Name is the provider name.
	Name string
	// URL is the API base URL.
	URL string
	// Token is the personal access token or app password.
	Token string
	// WebhookSecret validates incoming webhooks.
	WebhookSecret string
	// HTTPClient is the client to use.
	HTTPClient *http.Client
}

// GitHub is the GitHub forge implementation.
type GitHub struct {
	config Config
}

// NewGitHub creates a GitHub forge.
func NewGitHub(config Config) *GitHub {
	if config.URL == "" {
		config.URL = "https://api.github.com"
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHub{config: config}
}

// Name implements Forge.
func (g *GitHub) Name() string { return "github" }

// URL implements Forge.
func (g *GitHub) URL() string { return g.config.URL }

// Hook implements Forge.
func (g *GitHub) Hook(ctx context.Context, request *http.Request) (*HookRequest, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if !g.VerifySignature(body, request.Header.Get("X-Hub-Signature-256")) {
		return nil, fmt.Errorf("%w: the webhook signature is not valid", ErrNotFound)
	}

	var payload struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.Repository.FullName == "" {
		return nil, ErrIgnoreEvent
	}

	hook := &HookRequest{
		Event:       EventPush,
		Repository:  payload.Repository.FullName,
		Branch:      strings.TrimPrefix(payload.Ref, "refs/heads/"),
		Commit:      payload.After,
		PullRequest: payload.PullRequest.Number,
		Sender:      payload.Sender.Login,
		Payload:     body,
	}
	if payload.PullRequest.Number > 0 {
		hook.Event = EventPullRequest
		hook.Commit = payload.PullRequest.Head.SHA
		hook.Branch = strings.TrimPrefix(payload.PullRequest.Head.Ref, "refs/heads/")
	}
	if strings.HasPrefix(payload.Ref, "refs/tags/") {
		hook.Event = EventTag
	}
	return hook, nil
}

// VerifySignature validates an X-Hub-Signature-256 header.
func (g *GitHub) VerifySignature(body []byte, header string) bool {
	if g.config.WebhookSecret == "" {
		return false
	}
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(g.config.WebhookSecret))
	mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

// Branches implements Forge.
func (g *GitHub) Branches(ctx context.Context, token, repository string) ([]Branch, error) {
	var payload []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	endpoint := fmt.Sprintf("%s/repos/%s/branches", strings.TrimRight(g.config.URL, "/"), repository)
	if err := g.get(ctx, token, endpoint, &payload); err != nil {
		return nil, err
	}
	result := make([]Branch, 0, len(payload))
	for _, branch := range payload {
		result = append(result, Branch{Name: branch.Name, Head: branch.Commit.SHA})
	}
	return result, nil
}

// Pipeline implements Forge.
func (g *GitHub) Pipeline(ctx context.Context, token, repository, path, commit string) ([]byte, error) {
	if path == "" {
		path = ".barq.yaml"
	}
	endpoint := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s",
		strings.TrimRight(g.config.URL, "/"), repository, url.PathEscape(path), url.QueryEscape(commit))
	var payload struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := g.get(ctx, token, endpoint, &payload); err != nil {
		return nil, err
	}
	if payload.Encoding == "base64" {
		decoded, err := decodeBase64(payload.Content)
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}
	return []byte(payload.Content), nil
}

// Status implements Forge.
func (g *GitHub) Status(ctx context.Context, token, repository, commit string, status Status) error {
	endpoint := fmt.Sprintf("%s/repos/%s/statuses/%s",
		strings.TrimRight(g.config.URL, "/"), repository, url.PathEscape(commit))
	body, err := json.Marshal(status)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := g.config.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		content, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("github returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	return nil
}

// get performs an authenticated GET and decodes the response.
func (g *GitHub) get(ctx context.Context, token, endpoint string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := g.config.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		content, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("github returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// GitLab is the GitLab forge implementation.
type GitLab struct {
	config Config
}

// NewGitLab creates a GitLab forge.
func NewGitLab(config Config) *GitLab {
	if config.URL == "" {
		config.URL = "https://gitlab.com"
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitLab{config: config}
}

// Name implements Forge.
func (g *GitLab) Name() string { return "gitlab" }

// URL implements Forge.
func (g *GitLab) URL() string { return g.config.URL }

// Hook implements Forge.
func (g *GitLab) Hook(ctx context.Context, request *http.Request) (*HookRequest, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	// GitLab authenticates with a shared token rather than a payload
	// signature.
	provided := request.Header.Get("X-Gitlab-Token")
	if g.config.WebhookSecret == "" || !hmac.Equal([]byte(provided), []byte(g.config.WebhookSecret)) {
		return nil, fmt.Errorf("%w: the webhook token is not valid", ErrNotFound)
	}

	var payload struct {
		Ref         string `json:"ref"`
		CheckoutSHA string `json:"checkout_sha"`
		ProjectID   int    `json:"project_id"`
		ProjectPath string `json:"project_path_with_namespace"`
		User        struct {
			Username string `json:"username"`
		} `json:"user_username"`
		ObjectKind string `json:"object_kind"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.ProjectPath == "" {
		return nil, ErrIgnoreEvent
	}

	hook := &HookRequest{
		Event:      EventPush,
		Repository: payload.ProjectPath,
		Branch:     strings.TrimPrefix(payload.Ref, "refs/heads/"),
		Commit:     payload.CheckoutSHA,
		Sender:     payload.User.Username,
		Payload:    body,
	}
	switch payload.ObjectKind {
	case "merge_request":
		hook.Event = EventPullRequest
	case "tag_push":
		hook.Event = EventTag
	case "release":
		hook.Event = EventRelease
	}
	return hook, nil
}

// Branches implements Forge.
func (g *GitLab) Branches(ctx context.Context, token, repository string) ([]Branch, error) {
	var payload []struct {
		Name   string `json:"name"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches",
		strings.TrimRight(g.config.URL, "/"), url.PathEscape(repository))
	if err := g.get(ctx, token, endpoint, &payload); err != nil {
		return nil, err
	}
	result := make([]Branch, 0, len(payload))
	for _, branch := range payload {
		result = append(result, Branch{Name: branch.Name, Head: branch.Commit.ID})
	}
	return result, nil
}

// Pipeline implements Forge.
func (g *GitLab) Pipeline(ctx context.Context, token, repository, path, commit string) ([]byte, error) {
	if path == "" {
		path = ".barq.yaml"
	}
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/repository/files/%s/raw?ref=%s",
		strings.TrimRight(g.config.URL, "/"), url.PathEscape(repository),
		url.PathEscape(path), url.QueryEscape(commit))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		request.Header.Set("PRIVATE-TOKEN", token)
	}
	response, err := g.config.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		content, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("gitlab returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	return io.ReadAll(io.LimitReader(response.Body, 1<<20))
}

// Status implements Forge.
func (g *GitLab) Status(ctx context.Context, token, repository, commit string, status Status) error {
	endpoint := fmt.Sprintf("%s/api/v4/projects/%s/statuses/%s",
		strings.TrimRight(g.config.URL, "/"), url.PathEscape(repository), url.PathEscape(commit))
	body, err := json.Marshal(status)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("PRIVATE-TOKEN", token)
	}
	response, err := g.config.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		content, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("gitlab returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	return nil
}

// get performs an authenticated GET and decodes the response.
func (g *GitLab) get(ctx context.Context, token, endpoint string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("PRIVATE-TOKEN", token)
	}
	response, err := g.config.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		content, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("gitlab returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// decodeBase64 decodes a base64 payload.
func decodeBase64(content string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decode the file content: %w", err)
	}
	return decoded, nil
}
