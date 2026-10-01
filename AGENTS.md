# AGENTS.md

## Scope

These instructions apply to the standalone Go worker in
`tools/skill2api`. It runs Codex skills as asynchronous Periodic jobs and
persists job state under the configured output root.

## Repository Layout

- `main.go`: configuration, Periodic handlers, Codex execution, status storage,
  validation, and recovery.
- `main_test.go`: focused unit tests for validation, status persistence,
  interaction parsing, configuration, and Codex process handling.
- `README.md`: caller-facing Periodic protocol and examples.
- `skills/<name>/SKILL.md`: skills available to the worker.
- `output/<request-id>/`: generated artifacts and `status.json`; treat these as
  runtime output, not source.

## Development Commands

Run these from this directory:

```bash
gofmt -w main.go main_test.go
go test ./...
go build ./...
git diff --check
```

Use `go test ./...` for source-level verification. A passing build or unit test
does not prove that Periodic, the installed Codex CLI, or a generated skill
works end to end.

## Configuration

Required environment variables:

- `PERIODIC_PORT`: Periodic endpoint.
- `SKILL2API_OUTPUT_ROOT`: absolute root for all job directories.

Optional settings include:

- `SKILL2API_SKILLS_DIR` (defaults to `skills`)
- `SKILL2API_CODEX_BIN` (defaults to `codex`)
- `SKILL2API_CODEX_DOCKER_OPT_DIR` (optional host directory mounted read-only at container `/opt` in Docker mode; `/opt/bin` is added to `PATH`)
- `SKILL2API_CODEX_TIMEOUT_SECONDS` (defaults to 21600 / 6 hours)
- `SKILL2API_MAX_OUTPUT_BYTES` (defaults to 65536)
- `SKILL2API_DEBUG` (defaults to `false`; startup-only debug logging with private data)
- `TASK_PREFIX`
- `PERIODIC_RSA_MODE`
- `PERIODIC_RSA_PRIVATE_KEY_PATH`
- `PERIODIC_RSA_PUBLIC_KEY_PATH`

RSA key paths are required unless plain mode (`RSA mode 0`) is explicitly
selected. Do not commit credentials, private keys, `.env` files, generated
output, or the compiled `skill2api` binary.

## Periodic Contract

The worker registers these functions, optionally prefixed by `TASK_PREFIX`:

- `skill2api_generate`: queue a new generation request.
- `skill2api_status`: read the current status; the workload may be omitted and
  the job name is used as the request identity.
- `skill2api_resume`: answer a pending interactive question.

Use `job.Name` as the canonical `request_id`. A payload request ID, when
provided, must match it. A valid generation request has a safe `request_id`, a
`skill_name` resolving to `<skills-dir>/<skill_name>/SKILL.md`, an output
directory inside `SKILL2API_OUTPUT_ROOT`, and a non-empty prompt.

The normal caller flow is:

```bash
periodic run skill2api_generate request-1 \
  --workload '{"request_id":"request-1","skill_name":"myna-health-check","output_dir":"/path/to/output/request-1","prompt":"Generate the requested package.","force":false}' \
  --timeout 30
periodic run skill2api_status request-1 --timeout 30
```

Poll until `succeeded`, `failed`, or `waiting_for_input`. Do not infer a
percentage while a job is `running`; the worker exposes state and timestamps,
not fabricated progress.

Interactive skills must end Codex output with exactly:

```text
SKILL2API_INPUT_REQUIRED
{"question":"...","options":["..."]}
```

Resume with the same request ID and continue polling. The internal Codex
session ID is stored privately and is never returned by `skill2api_status`.

## State and Safety Invariants

- Status is stored at `SKILL2API_OUTPUT_ROOT/<request-id>/status.json`.
- Writes use a temporary file followed by rename; preserve this atomic update
  behavior.
- Duplicate queued or running IDs are rejected. `force` may replace a terminal
  request but must not interrupt active work.
- Running jobs are recovered as `failed` with `worker interrupted` after a
  worker restart.
- Keep path traversal checks and output-root containment checks intact.
- Codex is invoked as `codex exec --sandbox workspace-write --cd <output-dir>
  --skip-git-repo-check ...`; do not reintroduce obsolete flags such as
  `--full-auto`.
- Keep prompts and answers out of logs by default. Log lifecycle events and
  useful error context without leaking secrets.
- Keep public status responses free of `session_id`.
- When `SKILL2API_DEBUG=true`, worker-managed Codex logs and status log tails
  intentionally include private data; keep this mode limited to controlled
  environments. The public status schema still omits `session_id`.

## End-to-End Verification

When changing the Periodic or Codex execution path, use the existing shell
environment and run a real request with `periodic run`, then poll with
`periodic status` or `periodic run skill2api_status`. Inspect the terminal
`status.json`, generated file list, and relevant artifact contents. A socket
permission failure from a restricted sandbox is an environment limitation;
rerun with authorized connectivity before diagnosing application behavior.

Update `README.md` when changing the public workload schema, status states,
interactive marker, environment variables, or command examples. Keep changes
focused and preserve unrelated working-tree files and generated artifacts.
