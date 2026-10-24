package k8s

// WorkerScript is the POSIX shell entrypoint that every Barq worker runs.
//
// It performs four jobs:
//
//  1. Hydrate /workspace from the pre-signed upstream workspace URLs.
//  2. Apply the upstream .barq.env outputs as *data*. The file is parsed
//     and exported, never sourced, so shell metacharacters in an output
//     value cannot execute.
//  3. Run the user commands as a supervised child so that a SIGTERM from
//     a Spot reclaim can be intercepted.
//  4. On SIGTERM, checkpoint the workspace to the pre-signed upload URL
//     and exit with code 75 (EX_TEMPFAIL) so the controller can reschedule
//     the step on another node.
//
// The script requires /bin/sh, tar and either curl or wget in the worker
// image. The bundled barq/plugin-runtime image satisfies this contract.
const WorkerScript = `#!/bin/sh
set -u

WORKSPACE="${BARQ_WORKSPACE:-/workspace}"
OUTPUT_FILE="${BARQ_OUTPUT_FILE:-${WORKSPACE}/.barq.env}"
CHILD_PID=""
EXIT_STATUS=0
SPOT_PREEMPTED=0

log() {
  printf '[barq] %s\n' "$*"
}

# reject_env protects the worker from outputs that could change how the
# shell itself behaves.
reject_env() {
  case "$1" in
    PATH|LD_PRELOAD|LD_LIBRARY_PATH|IFS|BASH_ENV|ENV|SHELLOPTS|PROMPT_COMMAND|PS4)
      return 1
      ;;
  esac
  return 0
}

# apply_outputs parses the upstream output file as data. Values are never
# evaluated, so a value such as $(id) is inert.
apply_outputs() {
  [ -f "$OUTPUT_FILE" ] || return 0
  # Bound the file so a malicious or runaway producer cannot exhaust
  # the worker.
  size=$(wc -c < "$OUTPUT_FILE" 2>/dev/null || echo 0)
  if [ "$size" -gt 1048576 ]; then
    log "ignoring oversized output file (${size} bytes)"
    return 0
  fi
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      ''|'#'*) continue ;;
    esac
    key=${line%%=*}
    value=${line#*=}
    if [ "$key" = "$line" ]; then
      continue
    fi
    case "$key" in
      [A-Za-z_]*) ;;
      *) continue ;;
    esac
    case "$key" in
      *[!A-Za-z0-9_]*) continue ;;
    esac
    if reject_env "$key"; then
      continue
    fi
    export "$key=$value"
  done < "$OUTPUT_FILE"
  return 0
}

download_workspaces() {
  [ -n "${BARQ_WORKSPACE_DOWNLOADS:-}" ] || return 0
  mkdir -p "$WORKSPACE"
  printf '%s\n' "$BARQ_WORKSPACE_DOWNLOADS" | while IFS= read -r url; do
    [ -n "$url" ] || continue
    log "hydrating workspace"
    if command -v curl >/dev/null 2>&1; then
      curl -fsSL --retry 3 --retry-delay 1 -o "${WORKSPACE}/.barq-upstream.tar.gz" "$url" || {
        log "workspace download failed"
        exit 70
      }
    elif command -v wget >/dev/null 2>&1; then
      wget -q -O "${WORKSPACE}/.barq-upstream.tar.gz" "$url" || {
        log "workspace download failed"
        exit 70
      }
    else
      log "no curl or wget available in worker image"
      exit 69
    fi
    tar -xzf "${WORKSPACE}/.barq-upstream.tar.gz" -C "$WORKSPACE" || {
      log "workspace extraction failed"
      exit 70
    }
    rm -f "${WORKSPACE}/.barq-upstream.tar.gz"
  done
  return 0
}

upload_workspace() {
  [ "${BARQ_NO_WORKSPACE:-}" = "1" ] && return 0
  [ -n "${BARQ_WORKSPACE_UPLOAD:-}" ] || return 0
  archive="${WORKSPACE}/.barq-workspace.tar.gz"
  tar -czf "$archive" -C "$WORKSPACE" . 2>/dev/null || return 1
  if command -v curl >/dev/null 2>&1; then
    curl -fsS --retry 3 --retry-delay 1 -X PUT --upload-file "$archive" "$BARQ_WORKSPACE_UPLOAD" || return 1
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O /dev/null --method=PUT --body-file="$archive" "$BARQ_WORKSPACE_UPLOAD" || return 1
  else
    return 1
  fi
  rm -f "$archive"
  return 0
}

on_terminate() {
  SPOT_PREEMPTED=1
  log "received termination signal, checkpointing workspace"
  if [ -n "$CHILD_PID" ]; then
    kill -TERM "$CHILD_PID" 2>/dev/null || true
    wait "$CHILD_PID" 2>/dev/null || true
  fi
  upload_workspace || log "checkpoint upload failed"
  # 75 is EX_TEMPFAIL. The controller treats it as a reschedule request
  # rather than a build failure.
  exit 75
}

trap on_terminate TERM INT

mkdir -p "$WORKSPACE"
download_workspaces
apply_outputs

if [ "$#" -gt 0 ]; then
  for command in "$@"; do
    log "running: $command"
    sh -c "$command" &
    CHILD_PID=$!
    wait "$CHILD_PID"
    EXIT_STATUS=$?
    CHILD_PID=""
    if [ "$EXIT_STATUS" -ne 0 ]; then
      break
    fi
  done
else
  log "no command supplied, idling"
  while true; do
    sleep 3600 &
    CHILD_PID=$!
    wait "$CHILD_PID"
    EXIT_STATUS=$?
    CHILD_PID=""
  done
fi

if [ "$SPOT_PREEMPTED" -eq 1 ]; then
  exit 75
fi

upload_workspace || {
  log "final workspace upload failed"
  exit 74
}

log "step finished with status ${EXIT_STATUS}"
exit "$EXIT_STATUS"
`
