// Command barqctl is the Barq administration CLI.
//
// It applies configuration as code, inspects the controller, and manages
// users, secrets and alert destinations. Every command talks to the
// controller's REST API, so the CLI and the web UI share exactly the
// same authorisation and audit path.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "barqctl: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches a command.
func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	command := args[0]
	rest := args[1:]

	switch command {
	case "help", "-h", "--help":
		usage()
		return nil
	case "version", "--version":
		fmt.Println("barqctl", version)
		return nil
	case "apply":
		return applyConfig(rest)
	case "validate":
		return validateConfig(rest)
	case "runs":
		return listRuns(rest)
	case "get":
		return getRun(rest)
	case "logs":
		return streamLogs(rest)
	case "stop":
		return stopRun(rest)
	case "rebuild":
		return rebuildRun(rest)
	case "spawn":
		return spawnPipeline(rest)
	case "users":
		return usersCommand(rest)
	case "secrets":
		return secretsCommand(rest)
	case "alerts":
		return alertsCommand(rest)
	case "stats":
		return statsCommand(rest)
	case "health":
		return healthCommand(rest)
	case "login":
		return loginCommand(rest)
	}
	return fmt.Errorf("unknown command %q", command)
}

// loginCommand authenticates and prints the session details.
//
// The command is meant for scripting. It reads the password from the
// environment so that it never appears in the process arguments or in
// the shell history.
func loginCommand(args []string) error {
	opts, rest := resolve(args)
	email := os.Getenv("BARQ_EMAIL")
	password := os.Getenv("BARQ_PASSWORD")
	if len(rest) > 0 {
		email = rest[0]
	}
	if email == "" {
		return errors.New("the email is required; set BARQ_EMAIL or pass it as an argument")
	}
	if password == "" {
		return errors.New("the password is required; set BARQ_PASSWORD")
	}
	api := newClient(opts)
	var payload struct {
		User authUser `json:"user"`
	}
	if err := api.post("/api/v1/auth/login", map[string]any{
		"email": email, "password": password,
	}, &payload); err != nil {
		return err
	}
	fmt.Printf("signed in as %s (%s)\n", payload.User.Email, payload.User.Role)
	return nil
}

// authUser is the user representation returned by the API.
type authUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// version is stamped at build time.
var version = "dev"

// usage prints the help text.
func usage() {
	fmt.Fprint(os.Stderr, `barqctl - Barq CI administration

Usage:
  barqctl <command> [flags]

Commands:
  apply <file>        Apply a Barq configuration document
  validate <file>     Validate a configuration without applying it
  spawn <file>        Spawn a pipeline from a YAML file or stdin
  runs                List runs
  get <run-id>        Show a run
  logs <run-id>       Stream the logs of a run
  stop <run-id>       Stop a running pipeline
  rebuild <run-id>    Rebuild a run (full, failed, wave or step)
  users               Manage user accounts
  secrets             Manage the encrypted secret store
  alerts              Manage alert destinations
  stats               Show usage statistics
  health              Show the system health report

Global flags:
  --server <url>      The controller URL (default $BARQ_SERVER or http://127.0.0.1:8080)
  --token <token>     The API token (default $BARQ_TOKEN)
`)
}

// options are the resolved global flags.
type options struct {
	server string
	token  string
	csrf   string
	cookie string
}

// resolve parses the global flags.
func resolve(args []string) (options, []string) {
	opts := options{
		server: os.Getenv("BARQ_SERVER"),
		token:  os.Getenv("BARQ_TOKEN"),
		csrf:   os.Getenv("BARQ_CSRF"),
		cookie: os.Getenv("BARQ_COOKIE"),
	}
	if opts.server == "" {
		opts.server = "http://127.0.0.1:8080"
	}
	rest := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--server":
			if index+1 < len(args) {
				index++
				opts.server = args[index]
			}
		case "--token":
			if index+1 < len(args) {
				index++
				opts.token = args[index]
			}
		case "--csrf":
			if index+1 < len(args) {
				index++
				opts.csrf = args[index]
			}
		case "--cookie":
			if index+1 < len(args) {
				index++
				opts.cookie = args[index]
			}
		default:
			rest = append(rest, args[index])
		}
	}
	return opts, rest
}

// client performs API calls.
type client struct {
	server string
	token  string
	csrf   string
	cookie string
	http   *http.Client
}

// newClient creates an API client.
func newClient(opts options) *client {
	return &client{
		server: strings.TrimRight(opts.server, "/"),
		token:  opts.token,
		csrf:   opts.csrf,
		cookie: opts.cookie,
		http:   &http.Client{Timeout: 60 * time.Second},
	}
}

// do performs a request and decodes the JSON response.
func (c *client) do(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, c.server+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.csrf != "" {
		request.Header.Set("X-Barq-CSRF", c.csrf)
	}
	if c.cookie != "" {
		request.Header.Set("Cookie", c.cookie)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(content, &payload) == nil && payload.Error != "" {
			return errors.New(payload.Error)
		}
		return fmt.Errorf("the server returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	if out != nil {
		return json.Unmarshal(content, out)
	}
	return nil
}

// post is a convenience wrapper.
func (c *client) post(path string, body any, out any) error {
	return c.do(http.MethodPost, path, body, out)
}

// postDocument sends a raw document. The controller parses it, so the
// client does not have to understand the document format.
func (c *client) postDocument(path string, document []byte, out any) error {
	request, err := http.NewRequest(http.MethodPost, c.server+path, bytes.NewReader(document))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/yaml")
	request.Header.Set("Accept", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.csrf != "" {
		request.Header.Set("X-Barq-CSRF", c.csrf)
	}
	if c.cookie != "" {
		request.Header.Set("Cookie", c.cookie)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var payload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(content, &payload) == nil && payload.Error != "" {
			return errors.New(payload.Error)
		}
		return fmt.Errorf("the server returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	if out != nil {
		return json.Unmarshal(content, out)
	}
	return nil
}

// applyConfig applies a configuration document.
func applyConfig(args []string) error {
	opts, rest := resolve(args)
	if len(rest) < 1 {
		return errors.New("apply requires a file path")
	}
	path := rest[0]
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	api := newClient(opts)
	var result map[string]any
	if err := api.postDocument("/api/v1/config/apply?base="+url.QueryEscape(baseDir(rest[0])), raw, &result); err != nil {
		return err
	}
	applied, _ := result["applied"].([]any)
	fmt.Printf("applied %d resource(s) to %s\n", len(applied), opts.server)
	for _, item := range applied {
		if entry, ok := item.(map[string]any); ok {
			fmt.Printf("  %-10s %v\n", entry["kind"], entry["name"])
		}
	}
	return nil
}

// validateConfig validates a configuration without applying it.
func validateConfig(args []string) error {
	opts, rest := resolve(args)
	if len(rest) < 1 {
		return errors.New("validate requires a file path")
	}
	raw, err := os.ReadFile(rest[0])
	if err != nil {
		return err
	}
	api := newClient(opts)
	var result map[string]any
	if err := api.postDocument("/api/v1/config/validate?base="+url.QueryEscape(baseDir(rest[0])), raw, &result); err != nil {
		return err
	}
	if warnings, ok := result["warnings"].([]any); ok && len(warnings) > 0 {
		fmt.Println("warnings:")
		for _, warning := range warnings {
			fmt.Println("  " + fmt.Sprint(warning))
		}
	}
	fmt.Println("the configuration is valid")
	return nil
}

// spawnPipeline starts a pipeline.
func spawnPipeline(args []string) error {
	opts, rest := resolve(args)
	var raw []byte
	var err error
	if len(rest) > 0 && rest[0] != "-" {
		raw, err = os.ReadFile(rest[0])
	} else {
		raw, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, opts.server+"/api/v1/runs", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/yaml")
	if opts.token != "" {
		request.Header.Set("Authorization", "Bearer "+opts.token)
	}
	if opts.csrf != "" {
		request.Header.Set("X-Barq-CSRF", opts.csrf)
	}
	if opts.cookie != "" {
		request.Header.Set("Cookie", opts.cookie)
	}
	response, err := (&http.Client{Timeout: 60 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(response.Body)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("the server returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	var run struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(content, &run); err != nil {
		return err
	}
	fmt.Printf("started run %s (%s)\n", run.ID, run.Name)
	return nil
}

// listRuns lists the runs.
func listRuns(args []string) error {
	opts, _ := resolve(args)
	api := newClient(opts)
	var payload struct {
		Runs []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Status    string `json:"status"`
			CreatedAt string `json:"created_at"`
			Steps     map[string]struct {
				Status     string `json:"status"`
				DurationMS int64  `json:"duration_ms"`
			} `json:"steps"`
		} `json:"runs"`
	}
	if err := api.do(http.MethodGet, "/api/v1/runs", nil, &payload); err != nil {
		return err
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "RUN\tPIPELINE\tSTATUS\tSTEPS\tCREATED")
	for _, run := range payload.Runs {
		counts := map[string]int{}
		for _, step := range run.Steps {
			counts[step.Status]++
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			run.ID[:min(8, len(run.ID))], run.Name, run.Status, summarise(counts), run.CreatedAt)
	}
	return writer.Flush()
}

// summarise renders a status count map.
func summarise(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+strconv.Itoa(counts[key]))
	}
	return strings.Join(parts, ",")
}

// getRun shows a run.
func getRun(args []string) error {
	opts, rest := resolve(args)
	if len(rest) < 1 {
		return errors.New("get requires a run id")
	}
	api := newClient(opts)
	var run map[string]any
	if err := api.do(http.MethodGet, "/api/v1/runs/"+url.PathEscape(rest[0]), nil, &run); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(run, "", "  ")
	fmt.Println(string(encoded))
	return nil
}

// streamLogs streams the logs of a run.
func streamLogs(args []string) error {
	opts, rest := resolve(args)
	if len(rest) < 1 {
		return errors.New("logs requires a run id")
	}
	request, err := http.NewRequest(http.MethodGet, opts.server+"/api/v1/runs/"+url.PathEscape(rest[0])+"/logs", nil)
	if err != nil {
		return err
	}
	if opts.token != "" {
		request.Header.Set("Authorization", "Bearer "+opts.token)
	}
	if opts.cookie != "" {
		request.Header.Set("Cookie", opts.cookie)
	}
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		content, _ := io.ReadAll(response.Body)
		return fmt.Errorf("the server returned status %d: %s", response.StatusCode, strings.TrimSpace(string(content)))
	}
	scanner := newLineScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var payload string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err == nil {
				fmt.Print(payload)
			}
		}
	}
	return scanner.Err()
}

// lineScanner reads lines from a stream.
type lineScanner struct {
	reader *bufioReader
	line   string
	err    error
}

// newLineScanner creates a line scanner.
func newLineScanner(reader io.Reader) *lineScanner {
	return &lineScanner{reader: &bufioReader{reader: reader}}
}

// bufioReader is a minimal line reader over an io.Reader.
type bufioReader struct {
	reader io.Reader
	buffer []byte
}

// Scan advances to the next line.
func (s *lineScanner) Scan() bool {
	if s.err != nil {
		return false
	}
	chunk := make([]byte, 4096)
	count, err := s.reader.reader.Read(chunk)
	if count > 0 {
		s.reader.buffer = append(s.reader.buffer, chunk[:count]...)
	}
	if err != nil && len(s.reader.buffer) == 0 {
		s.err = err
		return false
	}
	index := bytes.IndexByte(s.reader.buffer, '\n')
	if index < 0 {
		if err != nil {
			s.err = err
			s.line = string(s.reader.buffer)
			s.reader.buffer = nil
			return len(s.line) > 0
		}
		return false
	}
	s.line = string(s.reader.buffer[:index])
	s.reader.buffer = s.reader.buffer[index+1:]
	return true
}

// Text returns the current line.
func (s *lineScanner) Text() string { return s.line }

// Err returns the first error.
func (s *lineScanner) Err() error { return s.err }

// stopRun stops a run.
func stopRun(args []string) error {
	opts, rest := resolve(args)
	if len(rest) < 1 {
		return errors.New("stop requires a run id")
	}
	api := newClient(opts)
	if err := api.post("/api/v1/runs/"+url.PathEscape(rest[0])+"/stop", map[string]any{}, nil); err != nil {
		return err
	}
	fmt.Println("stopped")
	return nil
}

// rebuildRun rebuilds a run.
func rebuildRun(args []string) error {
	opts, rest := resolve(args)
	if len(rest) < 1 {
		return errors.New("rebuild requires a run id")
	}
	mode := "full"
	if len(rest) > 1 {
		mode = rest[1]
	}
	payload := map[string]any{"mode": mode}
	if mode == "step" && len(rest) > 2 {
		payload["step_id"] = rest[2]
	}
	if mode == "wave" && len(rest) > 2 {
		wave, err := strconv.Atoi(rest[2])
		if err != nil {
			return fmt.Errorf("the wave must be a number: %w", err)
		}
		payload["wave"] = wave
	}
	api := newClient(opts)
	var run struct {
		ID string `json:"id"`
	}
	if err := api.post("/api/v1/runs/"+url.PathEscape(rest[0])+"/rebuild", payload, &run); err != nil {
		return err
	}
	fmt.Printf("rebuild started as run %s\n", run.ID)
	return nil
}

// usersCommand manages user accounts.
func usersCommand(args []string) error {
	opts, rest := resolve(args)
	api := newClient(opts)
	if len(rest) == 0 || rest[0] == "list" {
		var payload struct {
			Users []struct {
				ID      string `json:"id"`
				Email   string `json:"email"`
				Role    string `json:"role"`
				Enabled bool   `json:"disabled"`
			} `json:"users"`
		}
		if err := api.do(http.MethodGet, "/api/v1/users", nil, &payload); err != nil {
			return err
		}
		writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(writer, "ID\tEMAIL\tROLE\tSTATE")
		for _, user := range payload.Users {
			state := "active"
			if user.Enabled {
				state = "disabled"
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", user.ID[:min(8, len(user.ID))], user.Email, user.Role, state)
		}
		return writer.Flush()
	}
	switch rest[0] {
	case "role":
		if len(rest) < 3 {
			return errors.New("role requires a user id and a role")
		}
		body := map[string]any{"role": rest[2]}
		if err := api.do(http.MethodPatch, "/api/v1/users/"+url.PathEscape(rest[1]), body, nil); err != nil {
			return err
		}
		fmt.Println("role updated")
		return nil
	case "disable":
		if len(rest) < 2 {
			return errors.New("disable requires a user id")
		}
		body := map[string]any{"disabled": true}
		if err := api.do(http.MethodPatch, "/api/v1/users/"+url.PathEscape(rest[1]), body, nil); err != nil {
			return err
		}
		fmt.Println("user disabled")
		return nil
	case "enable":
		if len(rest) < 2 {
			return errors.New("enable requires a user id")
		}
		body := map[string]any{"disabled": false}
		if err := api.do(http.MethodPatch, "/api/v1/users/"+url.PathEscape(rest[1]), body, nil); err != nil {
			return err
		}
		fmt.Println("user enabled")
		return nil
	}
	return fmt.Errorf("unknown users subcommand %q", rest[0])
}

// secretsCommand manages the secret store.
func secretsCommand(args []string) error {
	opts, rest := resolve(args)
	api := newClient(opts)
	if len(rest) == 0 || rest[0] == "list" {
		var payload struct {
			Secrets []struct {
				Name        string `json:"name"`
				Version     int    `json:"version"`
				Fingerprint string `json:"fingerprint"`
			} `json:"secrets"`
		}
		if err := api.do(http.MethodGet, "/api/v1/secrets", nil, &payload); err != nil {
			return err
		}
		writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(writer, "NAME\tVERSION\tFINGERPRINT")
		for _, secret := range payload.Secrets {
			fmt.Fprintf(writer, "%s\tv%d\t%s\n", secret.Name, secret.Version, secret.Fingerprint)
		}
		return writer.Flush()
	}
	switch rest[0] {
	case "set":
		if len(rest) < 2 {
			return errors.New("set requires a name; the value is read from the environment")
		}
		name := rest[1]
		value := os.Getenv(name)
		if value == "" {
			return fmt.Errorf("the environment variable %s is not set", name)
		}
		body := map[string]any{"name": name, "value": value}
		if err := api.post("/api/v1/secrets", body, nil); err != nil {
			return err
		}
		fmt.Printf("secret %s stored\n", name)
		return nil
	case "delete":
		if len(rest) < 2 {
			return errors.New("delete requires a name")
		}
		if err := api.do(http.MethodDelete, "/api/v1/secrets/"+url.PathEscape(rest[1]), nil, nil); err != nil {
			return err
		}
		fmt.Println("secret deleted")
		return nil
	}
	return fmt.Errorf("unknown secrets subcommand %q", rest[0])
}

// alertsCommand manages alert destinations.
func alertsCommand(args []string) error {
	opts, rest := resolve(args)
	api := newClient(opts)
	if len(rest) == 0 || rest[0] == "list" {
		var payload struct {
			Destinations []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"destinations"`
		}
		if err := api.do(http.MethodGet, "/api/v1/alerts", nil, &payload); err != nil {
			return err
		}
		writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(writer, "ID\tNAME\tURL")
		for _, destination := range payload.Destinations {
			fmt.Fprintf(writer, "%s\t%s\t%s\n", destination.ID, destination.Name, destination.URL)
		}
		return writer.Flush()
	}
	switch rest[0] {
	case "add":
		if len(rest) < 2 {
			return errors.New("add requires a URL")
		}
		body := map[string]any{"url": rest[1], "name": rest[1]}
		if len(rest) > 2 {
			body["name"] = rest[2]
		}
		if err := api.post("/api/v1/alerts", body, nil); err != nil {
			return err
		}
		fmt.Println("destination added")
		return nil
	case "delete":
		if len(rest) < 2 {
			return errors.New("delete requires an id")
		}
		if err := api.do(http.MethodDelete, "/api/v1/alerts/"+url.PathEscape(rest[1]), nil, nil); err != nil {
			return err
		}
		fmt.Println("destination removed")
		return nil
	}
	return fmt.Errorf("unknown alerts subcommand %q", rest[0])
}

// statsCommand shows the usage statistics.
func statsCommand(args []string) error {
	opts, _ := resolve(args)
	api := newClient(opts)
	var snapshot map[string]any
	if err := api.do(http.MethodGet, "/api/v1/stats", nil, &snapshot); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(snapshot, "", "  ")
	fmt.Println(string(encoded))
	return nil
}

// healthCommand shows the system health.
func healthCommand(args []string) error {
	opts, _ := resolve(args)
	api := newClient(opts)
	var report map[string]any
	if err := api.do(http.MethodGet, "/api/v1/system/health", nil, &report); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(encoded))
	return nil
}

// baseDir returns the directory that relative paths in a
// configuration document are resolved against.
func baseDir(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Dir(path)
	}
	return filepath.Dir(absolute)
}

// min returns the smaller of two integers.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
