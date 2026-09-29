# Skill2API Usage

Skill2API runs Codex skills as asynchronous Periodic jobs. Submit a job, poll
`skill2api_status`, and resume the saved Codex session when the skill needs a
user decision.

## Prerequisites

- The Periodic service is running.
- The Skill2API worker registers `skill2api_generate`, `skill2api_status`, and
  `skill2api_resume`.
- `skill_name` resolves to `SKILL2API_SKILLS_DIR/<skill_name>/SKILL.md`.
- `output_dir` is inside `SKILL2API_OUTPUT_ROOT`.

The examples below use `request-1`. The Periodic job name and the payload's
`request_id` must match.

## Submit a Job

```bash
periodic run skill2api_generate request-1 \
  --workload '{
    "request_id": "request-1",
    "skill_name": "myna-health-check",
    "output_dir": "/path/to/SKILL2API_OUTPUT_ROOT/request-1",
    "prompt": "Generate the requested health-check package.",
    "force": false
  }' \
  --timeout 30
```

An accepted submission only means that the job was queued:

```json
{"request_id":"request-1","status":"queued"}
```

## Poll Status

```bash
periodic run skill2api_status request-1 --timeout 30
```

The status workload can be omitted; the worker derives `request_id` from the
job name. The normal states are:

| State | Meaning |
| --- | --- |
| `queued` | The job was created and is waiting to run. |
| `running` | Codex is executing the skill. |
| `waiting_for_input` | The skill asked a question and is waiting for an answer. |
| `succeeded` | The task completed; `files` lists the generated files. |
| `failed` | Execution failed; inspect `error`, `stdout`, and `stderr`. |
| `not_found` | No job exists for the requested ID. |

Poll every few seconds until the job reaches `succeeded`, `failed`, or
`waiting_for_input`. Skill2API does not fabricate percentage progress; while a
job is executing, the reliable progress signal is `running`.

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
selected value:

```bash
periodic run skill2api_resume request-1 \
  --workload '{"request_id":"request-1","answer":"v2"}' \
  --timeout 30
```

Resume returns immediately with `running`; continue polling:

```json
{"request_id":"request-1","status":"running"}
```

A job can enter `waiting_for_input` more than once. Use the same
`skill2api_resume` function for each question. The internal Codex session ID is
persisted but is never returned by the status function.

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
- A job interrupted while `running` is marked `failed` with
  `worker interrupted`.
- A job can only be resumed from `waiting_for_input`; duplicate resume requests
  return a conflict.
- The backend currently does not enforce that `answer` belongs to `options`.
  The caller should constrain the UI selection, and the skill should validate
  the answer after resuming.
- Status is stored at
  `SKILL2API_OUTPUT_ROOT/<request_id>/status.json`. Generated files and the
  status file are kept in the job's `output_dir`.
