# Barq Plugin SDK

A Barq plugin is a container image that performs one job. Barq does not
treat every arbitrary image as a plugin. A plugin must satisfy a
**documented runtime contract**, because Barq needs to capture the
workspace, redact logs and checkpoint on preemption.

## 1. The contract

A plugin image must:

1. Provide `/usr/local/bin/barq-plugin`, or declare its command through
   a `run:` list on the step.
2. Provide `/bin/sh`, `tar` and either `curl` or `wget`, because the
   worker wrapper uses them to hydrate and checkpoint the workspace.
3. Write outputs to `/workspace/.barq.env`.

The simplest way to satisfy the contract is to build on the plugin
runtime image:

```dockerfile
FROM barq/plugin-runtime:1.0.0
COPY my-plugin /usr/local/bin/barq-plugin
RUN chmod 0755 /usr/local/bin/barq-plugin
ENTRYPOINT ["/usr/local/bin/barq-plugin"]
```

## 2. Inputs

Plugin configuration arrives as environment variables.

| Variable | Meaning |
|---|---|
| `PLUGIN_<KEY>` | Every key in the step's `with:` block, uppercased with non-alphanumeric characters replaced by underscores |
| `BARQ_RUN_ID` | The run identifier |
| `BARQ_STEP_ID` | The compiled step identifier |
| `BARQ_WAVE` | The execution wave |
| `BARQ_ATTEMPT` | The zero-based attempt number |
| `BARQ_NODE_INDEX` | The shard index when `parallelism` is used |
| `BARQ_NODE_TOTAL` | The shard count when `parallelism` is used |
| `BARQ_WORKSPACE` | The workspace directory, `/workspace` by default |
| `BARQ_OUTPUT_FILE` | The output file, `/workspace/.barq.env` by default |
| `BARQ_NO_WORKSPACE` | `1` when the step disabled workspace capture |

Example:

```yaml
- id: security-scan
  plugin: acme/barq-trivy-plugin:v1.0.0
  with:
    severity: "CRITICAL,HIGH"
    fail_on_vuln: "true"
    webhook_url: "https://hooks.example.com/..."
```

produces:

```text
PLUGIN_SEVERITY=CRITICAL,HIGH
PLUGIN_FAIL_ON_VULN=true
PLUGIN_WEBHOOK_URL=https://hooks.example.com/...
```

## 3. Outputs

A plugin exports values to downstream steps by appending `KEY=VALUE`
lines to `/workspace/.barq.env`:

```sh
printf 'SCAN_STATUS=PASSED\n' >> /workspace/.barq.env
printf 'IMAGE_DIGEST=sha256:abc123\n' >> /workspace/.barq.env
```

### The output file is data, never code

Barq **parses** the output file. It never sources it and never passes
it to a shell. A value of `$(rm -rf /)` is stored as the literal
string `$(rm -rf /)`.

The parser enforces these rules:

- Variable names must match `[A-Za-z_][A-Za-z0-9_]*`.
- Reserved names are rejected: `PATH`, `LD_PRELOAD`, `LD_LIBRARY_PATH`,
  `IFS`, `BASH_ENV`, `ENV`, `SHELLOPTS`, `PROMPT_COMMAND`, `PS4`.
- The file is capped at 1 MiB.
- Values may not contain newlines, because the format is line oriented.

### Fan-in conflicts

When a step depends on several upstream steps, the workspaces are
extracted in the order of the sorted dependency identifiers and the
later value wins. This is deterministic, but it means two upstream
steps that export the same key will silently conflict. Use namespaced
keys such as `BUILD_IMAGE` and `TEST_IMAGE` to avoid ambiguity.

## 4. Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. The workspace is uploaded and the step is cached. |
| `1..74` | Failure. The step fails and downstream steps are cancelled. |
| `75` | Reserved for the controller. Do not return it. A worker that receives `SIGTERM` returns `75` by itself, and the controller interprets it as "reschedule me". |

## 5. Secrets

A step requests a secret by name:

```yaml
- id: publish
  image: alpine:3.20
  secrets: [NPM_TOKEN]
  run: npm publish
```

The controller looks the name up in the encrypted secret store and
injects the value into the worker environment under exactly that name.
Secret values are redacted from logs before they reach disk or a live
subscriber.

Secret **values** must never appear in the output file. The output
file is archived alongside the run and is readable by anyone who can
read the run.

## 6. Workspace

The workspace is `/workspace`. It is an `emptyDir` with a 10 GiB limit.

- Before the plugin runs, Barq downloads the workspaces of the
  upstream steps and extracts them into `/workspace`.
- After the plugin exits, Barq tars `/workspace` and uploads it to the
  pre-signed URL for this step.
- A downstream step receives the union of its upstream workspaces.

A plugin that does not need the workspace can declare `no_workspace:
true`, which skips both the download and the upload.

## 7. Built-in plugins

Barq ships built-in plugins that resolve to the `barq://` namespace.
They run inside the plugin runtime image and never depend on a
third-party image existing in a registry.

| URI | Purpose | Required inputs |
|---|---|---|
| `barq://slack` | Post a message to a Slack incoming webhook | `PLUGIN_WEBHOOK_URL`; optional `PLUGIN_CHANNEL`, `PLUGIN_MESSAGE` |
| `barq://webhook` | Post a JSON payload to an arbitrary endpoint | `PLUGIN_WEBHOOK_URL`; optional `PLUGIN_MESSAGE` |
| `barq://git-clone` | Clone a repository into the workspace | `PLUGIN_REPOSITORY`; optional `PLUGIN_REF`, `PLUGIN_DEPTH`, `PLUGIN_TOKEN`, `PLUGIN_TARGET` |
| `barq://artifact` | Package the workspace as a tarball | optional `PLUGIN_ARCHIVE` |
| `barq://audit` | Append an audit record to the workspace | optional `PLUGIN_AUDIT_FILE` |

`barq://git-clone` accepts a token through `PLUGIN_TOKEN` and passes it
to git through `GIT_ASKPASS`, so the credential never appears in the
clone URL or in the process arguments.

## 8. Writing a plugin in Go

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	channel := os.Getenv("PLUGIN_CHANNEL")
	if channel == "" {
		channel = "#ci-builds"
	}
	message := os.Getenv("PLUGIN_MESSAGE")
	if message == "" {
		message = fmt.Sprintf("Barq step %s completed at %s",
			os.Getenv("BARQ_STEP_ID"),
			time.Now().UTC().Format(time.RFC3339))
	}

	payload, err := json.Marshal(map[string]string{
		"channel": channel,
		"text":    message,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode payload:", err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Post(
		os.Getenv("PLUGIN_WEBHOOK_URL"),
		"application/json",
		bytes.NewReader(payload),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deliver payload:", err)
		os.Exit(1)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		fmt.Fprintf(os.Stderr, "deliver payload: status %d\n", response.StatusCode)
		os.Exit(1)
	}

	file, err := os.OpenFile("/workspace/.barq.env", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open output file:", err)
		os.Exit(1)
	}
	defer file.Close()
	fmt.Fprintf(file, "LAST_NOTIFIED_CHANNEL=%s\n", channel)
}
```

Note the details that matter: the HTTP client has a timeout, the
status code is checked, and the output file is appended to rather than
truncated so that two plugins in the same step do not erase each
other's outputs.

## 9. Conformance checklist

A plugin is conformant when:

- [ ] It runs as a non-root user.
- [ ] It has no writable dependency on `/bin/sh`, `tar`, `curl` or
      `wget` beyond what the runtime image provides.
- [ ] It reads its configuration from `PLUGIN_*` variables.
- [ ] It writes outputs only through `/workspace/.barq.env`.
- [ ] It never writes a secret value to the output file.
- [ ] It never returns exit code `75`.
- [ ] It is idempotent, or the step sets `cache: false`.
- [ ] It is pinned by digest, not by a mutable tag, in production.
