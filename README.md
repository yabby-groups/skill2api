# Skill2API Usage

Skill2API runs Codex skills as asynchronous Periodic jobs. Submit a job, poll
`skill2api_status`, and resume the saved Codex session when the skill needs a
user decision.

## Prerequisites

- The Periodic service is running.
- Each worker requires a unique, safe `SKILL2API_NODE_ID`. It registers the
  shared `skill2api_generate` entrypoint plus node-scoped follow-up functions:
  `skill2api_status:<node_id>`, `skill2api_file:<node_id>`,
  `skill2api_file_delivery:<node_id>`,
  `skill2api_file_delivery_status:<node_id>`, `skill2api_resume:<node_id>`,
  `skill2api_terminate:<node_id>`, and `skill2api_cleanup:<node_id>`.
  `TASK_PREFIX`, when set, precedes the base function name, for example
  `generation-skill2api_status:node-a`.
- `skill_name` is one skill name or an ordered comma-separated list. Each name
  resolves to `SKILL2API_SKILLS_DIR/<skill_name>/SKILL.md`.
- `output_dir` is a relative path inside `SKILL2API_OUTPUT_ROOT`.

The examples below use `request-1`. The Periodic job name and the payload's
`request_id` must match.

## Submit a Job

```bash
periodic run skill2api_generate request-1 \
  --workload '{
    "request_id": "request-1",
    "skill_name": "myna-health-check",
    "output_dir": "request-1",
    "prompt": "Generate the requested health-check package.",
    "environment": {
      "OPENAI_API_KEY": "provider-key-for-this-request"
    },
    "model": "gpt-6-sol",
    "force": false
  }' \
  --timeout 30
```

An accepted submission only means that the job was queued. Save its `node_id`:

```json
{"request_id":"request-1","status":"queued","node_id":"node-a"}
```

Validation failures return JSON with `status: "rejected"`, `request_id`,
`node_id`, and `error`. No generation is scheduled for a rejected submission.
The HTTP bridge preserves `request_id` on rejected or unconfirmed submissions.
An unconfirmed acknowledgement does not prove that generation was rejected;
check worker logs for that request ID before submitting another job.

All control and cleanup validation errors also complete with JSON containing
`request_id`, `status: "failed"`, and `error`; they never use a bare Periodic
failure reply. Response encoding failures return the same safe JSON envelope.
If the transport itself cannot send the reply, the worker logs `send_failed`;
the caller must treat that outcome as unconfirmed.

`skill2api_generate` is intentionally shared so Periodic can assign new work
to any available worker. All task state, generated files, logs, Codex sessions,
and cleanup remain local to that worker. Every later call for this request must
therefore use the returned node suffix. A node ID must be unique across workers;
there is no cross-node state replication or failover.

## Poll Status

```bash
periodic run skill2api_status:node-a request-1 --timeout 30
```

The status workload can be omitted; the worker derives `request_id` from the
job name. The normal states are:

| State | Meaning |
| --- | --- |
| `queued` | The job was created and is waiting to run. |
| `running` | Codex is executing the skill. |
| `interrupted` | Worker restarted or Docker execution ended before Codex emitted its completion summary; when a session was saved, it can be resumed. |
| `waiting_for_input` | The skill asked a question and is waiting for an answer. |
| `succeeded` | The task completed and Codex emitted its token-usage completion summary; `files` lists files by modification time, oldest first. |
| `failed` | Execution failed; inspect `error`, `stdout`, and `stderr`. |
| `terminated` | The task was stopped by an explicit terminate request. |
| `not_found` | No job exists for the requested ID. |

Poll every few seconds until the job reaches `succeeded`, `failed`,
`waiting_for_input`, or `terminated`. Skill2API does not fabricate percentage progress; while a
job is executing, the reliable progress signal is `running`. Each status query
also reads the current tails of `stdout.log` and `stderr.log` from the job
directory into the response's `stdout` and `stderr` fields. Each field contains
at most `SKILL2API_MAX_OUTPUT_BYTES` bytes (64 KiB by default); when older
output is omitted, the field starts with `[earlier output truncated]`.

Codex writes complete output directly to
`SKILL2API_OUTPUT_ROOT/<request_id>/stdout.log` and `stderr.log`. These files
are updated while Codex runs. The worker does not rewrite `status.json` for
output changes: `skill2api_status` reads log tails and rebuilds `files` from
the current output directory without persisting either update.
Caller-provided prompts and resume answers are redacted before either log is
written. Codex session IDs are also redacted from public logs and status
responses; the worker retains the ID privately for resume.

For controlled troubleshooting, set `SKILL2API_DEBUG=true` before starting the
worker. Debug mode preserves caller prompts, resume answers, environment values,
and Codex session IDs in the worker-managed stdout/stderr logs and in the log
tails returned by `skill2api_status`. The `session_id` field itself remains
hidden from public status responses. Debug mode is read only at startup and
should be used only where these logs are access-controlled.

## Get a Generated File

Read one existing file from a task directory with its relative path. The task
may be in any state. The successful response is the file's raw binary bytes,
not JSON or Base64; use `file_path` to retain the filename.

```bash
periodic run skill2api_file:node-a request-1 \
  --workload '{"request_id":"request-1","file_path":"package/output.png"}' \
  --timeout 30 | tail -c +9 > output.png
```

The worker accepts regular files anywhere below the task output directory,
including `status.json`, `stdout.log`, and `stderr.log`. It rejects absolute
paths, traversal outside the directory, directories, and symbolic links that
resolve outside the directory. Errors are JSON objects with `request_id`,
`status`, and `error` fields. The Periodic CLI displays successful job data
with an eight-byte `Result: ` prefix; `tail -c +9` skips it. Periodic API
clients receive the unencoded job data directly.

`SKILL2API_MAX_FILE_BYTES` limits one read and defaults to 67108864 bytes
(64 MiB). Files over the limit are rejected without returning partial data.
`SKILL2API_FILE_UPLOAD_TIMEOUT_SECONDS` controls the per-attempt timeout for
temporary uploads and defaults to 45 seconds. Increase it when the upstream
file service needs longer to accept large media; retries remain limited to
three attempts.

## Asynchronous Temporary Delivery

`skill2api_file_delivery` uploads one task output without holding an HTTP
caller open. Its Periodic job name and `delivery_id` must match; its workload
also includes the source `request_id`, `file_path`, and worker-only
`environment`.

Before transferring bytes, the worker derives the Myna-compatible `file_key`
from the output and resolves any unexpired temporary object with that key.
Resolved content is reused globally. Otherwise the worker attempts the
temporary upload up to three times, with the configured per-attempt timeout.

Poll `skill2api_file_delivery_status` with the same delivery job name and a
workload containing `request_id` and `delivery_id`. It returns `queued`,
`running`, `succeeded`, or `failed`; successful responses include
the temporary file metadata and relative download URL. Delivery state is kept
in the source request's internal delivery directory and is removed with the
normal request cleanup.

## Per-request Environment and Model

To configure uv without changing requests or skills, set
`UV_PROJECT_ENVIRONMENT` in the worker's startup environment. Docker mode
forwards it to the container; use a writable container path such as
`/workspace/.venv`. Native mode inherits it from the worker environment.
This does not bypass skill resource validation: existing virtual environments
with external symlinks must still be kept outside the skill package.

`skill2api_generate` accepts an optional `environment` object of environment
variable names to values. Its values are passed only to the Codex subprocess;
they are never saved in `status.json` or returned by `skill2api_status`.
Values are redacted from the worker-managed stdout and stderr logs. The caller
must provide any required variables again on every `skill2api_resume` request,
because the worker does not persist them.

`model` is an optional string on `skill2api_generate`. The worker passes it to
`codex exec --model`, stores it with the task, includes it in status responses,
and uses the same model for every resume. `skill2api_resume` does not accept a
model override.

## Docker Codex Runner

Set `SKILL2API_CODEX_DOCKER=true` to run Codex in a one-shot Docker container
instead of invoking the worker host's `codex` binary. The default image is
`lupino/sandbox-runner:latest`; override it with
`SKILL2API_CODEX_DOCKER_IMAGE`. `SKILL2API_CODEX_DOCKER_BIN` selects the Docker
CLI binary and defaults to `docker`.

The worker mounts the task output directory at `/workspace` and a private,
per-request Codex home at `/home/ubuntu`. Docker runs as the host UID/GID so
Codex can write that bind mount. The home persists across `skill2api_resume`
calls and is not included in the task's `files` response. The image startup
script executes `codex exec ...`; Codex runs in the container with
`--sandbox danger-full-access`; do not mount the Docker socket into this
runner. Each container is named `skill2api-<request_id>` and uses Docker's
`--rm` cleanup. `skill2api_terminate` force-removes that named container after
cancelling the worker process. The prefix keeps Docker names valid when a
request ID starts with `.`, `_`, or `-`.

Only selected skill packages are also mounted, read-only, at
`/skill2api/skills/<skill_name>`, outside the task output mount. This avoids
Docker creating root-owned mount target directories in the task output on the
host. A multi-skill request uses an ordered,
comma-separated `skill_name`, for example `"hypit,imagegen"`; Codex receives
each selected `SKILL.md` in that order and a mapping to its package path.
These namespaced mounts keep same-named resource directories such as
`references/` independent. The worker does not mount the parent `skills/`
directory or unselected packages. Existing single-skill requests remain valid.

Before every Docker invocation, the worker writes the following
provider configuration to the task's private
`/home/ubuntu/.codex/config.toml`:

```toml
sandbox_mode = "danger-full-access"
model_provider = "huabot"
model = "gpt-6-luna"

[model_providers.huabot]
name = "Huabot API"
base_url = "https://huabot.com/v1"
wire_api = "responses"
env_key = "OPENAI_API_KEY"
supports_websockets = false

[projects."/workspace"]
trust_level = "trusted"
```

Docker runs use the Docker default network so this provider is reachable. A
request `model` remains an explicit Codex command-line override of this
configuration default.

Optionally set `SKILL2API_CODEX_DOCKER_OPT_DIR` to an existing host directory
of externally managed tools. When Docker mode is enabled, the worker mounts it
read-only at `/opt` and adds `/opt/bin` ahead of the standard container paths.
The Docker UID/GID must be able to read and execute every required directory
and executable. Leave this variable unset to retain the default mounts and
container `PATH`.

Pass `OPENAI_API_KEY` in the request `environment` object on every generation
and resume. This request value takes precedence. When omitted, the worker's
`OPENAI_API_KEY` environment variable is used as a deployment-level fallback.
Neither source is persisted or returned. `SANDBOX_AI_KEY` is no longer used;
rename it to `OPENAI_API_KEY` in deployments and caller workloads.

`OPENAI_BASE_URL` selects the model API endpoint for generation and resume.
A non-empty request `environment.OPENAI_BASE_URL` takes precedence over the
worker environment variable; blank or omitted values fall back to the worker
value, then to `https://huabot.com/v1`. The effective URL is passed to skill
processes and written as the Docker Codex provider's `base_url` before each run.
The TOML example above shows the default URL. File uploads continue to use
`SKILL2API_UPLOAD_BASE_URL` and require `environment.OPENAI_API_KEY` in the
upload request.

## Codex Execution Timeout

`SKILL2API_CODEX_TIMEOUT_SECONDS` bounds each Codex execution, including an
execution resumed after an interactive prompt. It defaults to 21600 seconds
(6 hours), which accommodates long-running video-generation skills. Set it to
a larger positive number when a deployment needs a longer limit.

Set `SKILL2API_DEBUG=true` to disable privacy redaction in worker-managed Codex
logs and status log tails. It is read when the worker starts and defaults to
`false`.

## Direct Provider Network

Set `SKILL2API_CODEX_NO_PROXY=true` when Codex skills must reach providers
directly. The worker then removes `http_proxy`, `https_proxy`, `all_proxy`, and
`no_proxy` from the Codex subprocess environment. The default is `false`.

Native Codex runs always receive the `workspace-write` network-access setting.
Docker Codex runs always use the Docker default network for the configured
Sandbox Runner provider. Outbound network access is not configurable.

## Terminate a Job

Terminate a queued, running, or waiting-for-input task with the same request ID:

```bash
periodic run skill2api_terminate:node-a request-1 --timeout 30
```

The response is terminal and the task can no longer be resumed:

```json
{"request_id":"request-1","status":"terminated","error":"terminated by user"}
```

For a running task, the worker cancels the Codex process. Generated files are
kept; termination only updates the task state.

## Cleanup Expired Tasks

Skill2API submits this function before each `generate` or `resume` execution.
It is delayed for 24 hours, and its Periodic job name is the canonical request
ID. It can also be invoked manually for one request:

```bash
periodic run skill2api_cleanup:node-a request-1 --timeout 30
```

It deletes `succeeded`, `failed`, and `terminated` tasks, including
`status.json`, logs, generated files, and the private per-request Codex home
containing the resumable session. If the task is still active when cleanup runs,
the cleanup job uses `SchedLater(86400)` and checks again one day later.

Cleanup only deletes the documented dedicated layout where `output_dir` equals
`request_id`. Custom output directories are skipped to avoid deleting shared
data. The response contains the `request_id` and a status of `deleted`,
`not_due`, `deferred`, `skipped`, `not_found`, or `failed`.

## Interactive Protocol

When a skill needs user information, it must end its response with these two
lines. The first line must match exactly, and the second line must be JSON:

```text
SKILL2API_INPUT_REQUIRED
{"question":"Choose an API version","options":["v1","v2"]}
```

`options` is optional. The status response includes the question and options:

```json
{
  "request_id": "request-1",
  "status": "waiting_for_input",
  "phase": "clarification",
  "question": "Choose an API version",
  "options": ["v1", "v2"]
}
```

The caller can render `options` as buttons or a select control, then submit the
selected value. The same `skill2api_resume` endpoint also restores an
`interrupted` request after a worker restart.

```bash
periodic run skill2api_resume:node-a request-1 \
  --workload '{"request_id":"request-1","answer":"v2","environment":{"OPENAI_API_KEY":"provider-key-for-this-request"}}' \
  --timeout 30
```

To continue an interrupted task without adding instructions, omit both `answer`
and `instruction`:

```bash
periodic run skill2api_resume:node-a request-1 --timeout 30
```

To continue an interrupted or completed task with a follow-up instruction that
may add or modify files in the existing output directory, send `instruction`
instead. A completed task requires a non-empty instruction:

```bash
periodic run skill2api_resume:node-a request-1 \
  --workload '{"request_id":"request-1","instruction":"Add error handling and update README."}' \
  --timeout 30
```

Resume returns immediately with `running`; continue polling:

```json
{"request_id":"request-1","status":"running"}
```

A job can enter `waiting_for_input` more than once. Use the same
`skill2api_resume` function for each question. The internal Codex session ID is
persisted but is never returned by the status function.

Skills must use this exact two-line marker when a zero-exit execution still
requires caller action before the requested deliverable can be produced:

```text
SKILL2API_INPUT_REQUIRED
{"question":"Provide the missing source video","options":["submit source video"]}
```

This returns `waiting_for_input`; ordinary natural-language statements about a
missing credential, source, approval, or decision do not change a successful
Codex process exit into a non-terminal task state.

## Skill Authoring Guidance

- Use stable, short values in `options`.
- Make the question specific enough to display directly to a user.
- Do not guess when an answer is required, and do not write files that depend on
  the answer before pausing.
- The answer is passed as a string in `answer`. For multiple fields, pass a JSON
  string and let the skill validate and parse it.
- Ordinary text does not pause a job. The exact marker and valid JSON are
  required.

## Restart, Duplicate, and Failure Handling

- A `waiting_for_input` job survives a worker restart and can be resumed.
- A running job with a saved Codex session is marked `interrupted` after a
  worker restart and can be resumed through `skill2api_resume`. A job whose
  session ID was not yet saved is marked `failed` with `worker interrupted`.
- `skill2api_resume` requires `answer` for `waiting_for_input`; for
  `interrupted`, `failed`, and `terminated`, `instruction` is optional. A
  `succeeded` task can be resumed only with a non-empty `instruction`. Do not
  provide both fields in one request. Duplicate resume requests return a
  conflict. Any resumed task requires a saved Codex session ID.
- The backend currently does not enforce that `answer` belongs to `options`.
  The caller should constrain the UI selection, and the skill should validate
  the answer after resuming.
- Status is stored at
  `SKILL2API_OUTPUT_ROOT/<request_id>/status.json`. Generated files, the status
  file, and `stdout.log`/`stderr.log` are kept in the job's `output_dir`.
- A resumed task appends to its existing log files. A new generation or a
  `force` rerun truncates them before Codex starts. A failed or terminated task
  without a saved session ID cannot be resumed and needs a deliberate `force`
  rerun. If Codex cannot restore a saved session, the task becomes `failed`;
  generated files and logs remain for inspection.
- Native and Docker Codex runs both use a private per-request home under
  `SKILL2API_OUTPUT_ROOT/.skill2api-codex/<request_id>/home`. It retains the
  Codex session until cleanup and is never exposed through task status.
