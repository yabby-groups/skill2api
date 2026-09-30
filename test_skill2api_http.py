#!/usr/bin/env python3
"""End-to-end HTTP smoke test for Huabot Skill2API."""

from __future__ import annotations

import argparse
import json
import mimetypes
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from pathlib import Path
from typing import Any
import os


DEFAULT_BASE_URL = "https://huabot.com"
DEFAULT_CLIENT_ID = "kRP-pREgGsmqLNlFeWKiJDGmS7JbSJob"
SCOPES = "files:upload skill2api offline_access"


def get_shard_relative_path(file_key: str) -> str:
    """Build the two-level relative storage shard for a file key.

    Args:
        file_key: Content-derived file identifier.

    Returns:
        The relative shard path, or an empty string when the key is too short.
    """
    if len(file_key) < 4:
        return ''
    return os.path.join(file_key[:2], file_key[2:4])


def request_json(
    url: str,
    *,
    method: str = "GET",
    headers: dict[str, str] | None = None,
    body: bytes | None = None,
    timeout: int = 300,
) -> dict[str, Any]:
    request = urllib.request.Request(
        url, data=body, headers=headers or {}, method=method
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"HTTP {exc.code} {url}: {detail}") from exc
    try:
        value = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"Expected JSON from {url}") from exc
    if not isinstance(value, dict):
        raise RuntimeError(f"Expected JSON object from {url}")
    return value


def form_body(fields: dict[str, str]) -> bytes:
    return urllib.parse.urlencode(fields).encode("utf-8")


def upload_file(base_url: str, token: str, path: Path) -> dict[str, Any]:
    content = path.read_bytes()
    mime = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
    boundary = "----skill2api-python-boundary"
    parts = [
        (
            f"--{boundary}\r\n"
            'Content-Disposition: form-data; name="temporary"\r\n\r\n'
            "true\r\n"
        ).encode(),
        (
            f"--{boundary}\r\n"
            f'Content-Disposition: form-data; name="file"; filename="{path.name}"\r\n'
            f"Content-Type: {mime}\r\n\r\n"
        ).encode(),
        content,
        f"\r\n--{boundary}--\r\n".encode(),
    ]
    return request_json(
        f"{base_url}/api/file/run/",
        method="POST",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": f"multipart/form-data; boundary={boundary}",
        },
        body=b"".join(parts),
        timeout=180,
    )


def temporary_download_url(
    base_url: str,
    token: str,
    request_id: str,
    path: str,
) -> str:
    """Request a worker-uploaded temporary download URL.

    Args:
        base_url: Myna HTTP API origin.
        token: OAuth access token for the owning Skill2API request.
        request_id: Generated Skill2API request identifier.
        path: Worker-approved relative output path.

    Returns:
        Relative temporary upload URL returned by the Skill2API file endpoint.

    Raises:
        RuntimeError: If the worker response has no safe temporary upload URL.
    """
    query = urllib.parse.urlencode({"request_id": request_id, "file_path": path})
    payload = request_json(
        f"{base_url}/api/skill2api/file/?{query}",
        headers={"Authorization": f"Bearer {token}"},
    )
    url = payload.get("url")
    parsed = urllib.parse.urlsplit(url if isinstance(url, str) else "")
    if parsed.scheme or parsed.netloc or not parsed.path.startswith("/upload/"):
        raise RuntimeError(f"Invalid temporary download URL for {path}")
    return url


def download_file(base_url: str, url: str, output: Path) -> None:
    """Download one temporary upload URL to a local output path.

    Args:
        base_url: Myna HTTP API origin.
        url: Relative temporary upload URL.
        output: Local destination path.
    """
    request = urllib.request.Request(urllib.parse.urljoin(f"{base_url}/", url))
    try:
        with urllib.request.urlopen(request, timeout=180) as response:
            output.write_bytes(response.read())
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"Download failed for {output}: HTTP {exc.code}: {detail}") from exc


def choose_answer(question: Any, options: Any) -> str:
    """Read a non-empty interactive answer, constraining listed options locally."""
    prompt = str(question) if isinstance(question, str) and question.strip() else "Task needs input"
    print(f"\n{prompt}")
    choices = [option for option in options if isinstance(option, str)] if isinstance(options, list) else []
    if choices:
        for index, option in enumerate(choices, start=1):
            print(f"  {index}. {option}")
        while True:
            try:
                answer = input(f"Select an option [1-{len(choices)}]: ").strip()
            except EOFError as exc:
                raise RuntimeError("Input stream closed while selecting an option") from exc
            if answer.isdigit() and 1 <= int(answer) <= len(choices):
                return choices[int(answer) - 1]
            if answer in choices:
                return answer
            print("Enter an option number or its exact value.", file=sys.stderr)

    while True:
        try:
            answer = input("Answer: ").strip()
        except EOFError as exc:
            raise RuntimeError("Input stream closed while entering an answer") from exc
        if answer:
            return answer
        print("An answer is required.", file=sys.stderr)


def resume_request(base_url: str, token: str, request_id: str, answer: str) -> dict[str, Any]:
    """Submit one answer to a pending Skill2API clarification."""
    return request_json(
        f"{base_url}/api/skill2api/resume/",
        method="POST",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        body=json.dumps({"request_id": request_id, "answer": answer}).encode(),
    )


def get_status(base_url: str, token: str, request_id: str) -> dict[str, Any]:
    """Fetch the public status for one Skill2API request."""
    query = urllib.parse.urlencode({"request_id": request_id})
    return request_json(
        f"{base_url}/api/skill2api/status/?{query}",
        headers={"Authorization": f"Bearer {token}"},
    )


def answer_pending_request(
    base_url: str,
    token: str,
    request_id: str,
    status: dict[str, Any],
    answers: list[str],
    answer_index: int,
) -> int:
    """Choose one pending answer, submit it, and return the next answer index."""
    if answer_index < len(answers):
        answer = answers[answer_index]
        print("Answering pending question from --answer.")
    else:
        answer = choose_answer(status.get("question"), status.get("options"))
    resumed = resume_request(base_url, token, request_id, answer)
    print(f"resume status={resumed.get('status')}")
    return answer_index + 1


def poll_until_terminal(
    base_url: str,
    token: str,
    request_id: str,
    *,
    poll_seconds: int,
    poll_timeout: int,
    answers: list[str],
) -> dict[str, Any]:
    """Poll a request, interactively resuming each pending clarification."""
    deadline = time.monotonic() + poll_timeout
    terminal = {"succeeded", "failed", "terminated"}
    answer_index = 0
    while True:
        status = get_status(base_url, token, request_id)
        state = status.get("status")
        print(f"status={state}")
        if state == "waiting_for_input":
            answer_index = answer_pending_request(
                base_url, token, request_id, status, answers, answer_index
            )
            time.sleep(poll_seconds)
            continue
        if state in terminal:
            return status
        if time.monotonic() >= deadline:
            raise RuntimeError("Skill2API polling timed out")
        time.sleep(poll_seconds)


def parse_arguments() -> argparse.Namespace:
    """Parse and validate the local media and execution options."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("video", type=Path)
    parser.add_argument("image", type=Path)
    parser.add_argument("--base-url", default=DEFAULT_BASE_URL)
    parser.add_argument("--client-id", default=DEFAULT_CLIENT_ID)
    parser.add_argument("--model", default="qwen3.8-flash")
    parser.add_argument("--output", type=Path, default=Path("skill2api-result"))
    parser.add_argument("--poll-seconds", type=int, default=5)
    parser.add_argument("--poll-timeout", type=int, default=1800)
    parser.add_argument(
        "--answer",
        action="append",
        default=[],
        metavar="ANSWER",
        help="answer pending questions in order before prompting in the terminal",
    )
    args = parser.parse_args()
    if not args.video.is_file() or not args.image.is_file():
        parser.error("video and image must be existing files")
    if args.poll_seconds <= 0 or args.poll_timeout <= 0:
        parser.error("poll-seconds and poll-timeout must be positive")
    return args


def poll_device_token(device: dict[str, Any], token_endpoint: str, client_id: str) -> str:
    """Wait for the browser-approved OAuth device grant."""
    deadline = time.monotonic() + int(device.get("expires_in", 600))
    interval = max(3, int(device.get("interval", 3)))
    while time.monotonic() < deadline:
        time.sleep(interval)
        try:
            response = request_json(
                token_endpoint,
                method="POST",
                headers={"Content-Type": "application/x-www-form-urlencoded"},
                body=form_body(
                    {
                        "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
                        "client_id": client_id,
                        "device_code": device["device_code"],
                    }
                ),
            )
        except RuntimeError as exc:
            if "authorization_pending" in str(exc):
                continue
            raise
        if response.get("error") == "authorization_pending":
            continue
        if response.get("error"):
            raise RuntimeError(f"OAuth failed: {response['error']}")
        token = str(response["access_token"])
        print(f"OAuth authorized; scope={response.get('scope', '')}")
        return token
    raise RuntimeError("OAuth authorization timed out")


def authorize_device(base_url: str, client_id: str) -> tuple[str, str | None]:
    """Start browser device authorization and return its access token."""
    metadata = request_json(f"{base_url}/.well-known/oauth-authorization-server")
    device = request_json(
        metadata["device_authorization_endpoint"],
        method="POST",
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        body=form_body(
            {"client_id": client_id, "scope": SCOPES, "completion_action": "close"}
        ),
    )
    verification_url = device.get("verification_uri_complete") or device["verification_uri"]
    print(f"Open and approve OAuth authorization:\n{verification_url}")
    webbrowser.open(verification_url)
    token = poll_device_token(device, metadata["token_endpoint"], client_id)
    revoke_endpoint = metadata.get("revocation_endpoint")
    return token, revoke_endpoint if isinstance(revoke_endpoint, str) else None


def uploaded_file_url(base_url: str, upload: dict[str, Any]) -> str:
    """Turn a temporary upload response into its public provider URL."""
    file = upload["file"]
    file_key = str(file["file_key"])
    file_ext = str(file["file_ext"])
    shard = get_shard_relative_path(file_key)
    return f"{base_url}/upload/{shard}/{file_key}.{file_ext}"


def upload_source_media(base_url: str, token: str, image: Path, video: Path) -> tuple[str, str]:
    """Upload image and video sources, returning their temporary provider URLs."""
    image_upload = upload_file(base_url, token, image)
    video_upload = upload_file(base_url, token, video)
    image_url = uploaded_file_url(base_url, image_upload)
    video_url = uploaded_file_url(base_url, video_upload)
    print(f"Uploaded temporary image id={image_upload['file'].get('id')} {image_url}")
    print(f"Uploaded temporary video id={video_upload['file'].get('id')} {video_url}")
    return image_url, video_url


def build_hypit_prompt(video_url: str, image_url: str) -> str:
    """Build the requested Hypit clone instruction from uploaded source URLs."""
    return f"""
$hypit 克隆视频 {video_url} 替换商品为 {image_url} 替换人物为 公开的虚拟人像: asset://asset-20260720205609-dvhxr

尺寸 480p  9:16

预算 2 美金

"""


def submit_generation(base_url: str, token: str, prompt: str, model: str) -> str:
    """Create the Hypit Skill2API request and return its server-generated ID."""
    generated = request_json(
        f"{base_url}/api/skill2api/generate/",
        method="POST",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        body=json.dumps({"prompt": prompt, "skill_name": "hypit", "model": model}).encode(),
    )
    request_id = str(generated["request_id"])
    print(f"Submitted request_id={request_id}, model={model}")
    return request_id


def download_results(
    base_url: str,
    token: str,
    request_id: str,
    status: dict[str, Any],
    output: Path,
) -> None:
    """Download the last-created MP4 reported by the successful status response."""
    output.mkdir(parents=True, exist_ok=True)
    files = status.get("files") or []
    final_mp4 = next(
        (
            relative_path
            for relative_path in reversed(files)
            if isinstance(relative_path, str) and relative_path.lower().endswith(".mp4")
        ),
        None,
    )
    if final_mp4 is None:
        raise RuntimeError("Skill2API succeeded but status reported no MP4 result")

    print(final_mp4)
    destination = output / final_mp4
    destination.parent.mkdir(parents=True, exist_ok=True)
    url = temporary_download_url(base_url, token, request_id, final_mp4)
    print(url)
    download_file(base_url, url, destination)
    print(f"Succeeded; downloaded final MP4 to {destination}")


def revoke_token(base_url: str, revoke_endpoint: str | None, client_id: str, token: str) -> None:
    """Best-effort OAuth cleanup that cannot mask the task result."""
    try:
        request_json(
            revoke_endpoint or f"{base_url}/oauth/revoke",
            method="POST",
            headers={"Content-Type": "application/x-www-form-urlencoded"},
            body=form_body({"client_id": client_id, "token": token}),
        )
        print("OAuth token revoked")
    except Exception as exc:  # pragma: no cover - cleanup must not hide the result
        print(f"Warning: token revocation failed: {exc}", file=sys.stderr)


def main() -> int:
    """Run the authenticated upload, generation, interaction, and download flow."""
    args = parse_arguments()

    base_url = args.base_url.rstrip("/")
    token: str | None = None
    revoke_endpoint: str | None = None
    try:
        token, revoke_endpoint = authorize_device(base_url, args.client_id)
        image_url, video_url = upload_source_media(base_url, token, args.image, args.video)
        prompt = build_hypit_prompt(video_url, image_url)
        print(prompt)
        request_id = submit_generation(base_url, token, prompt, args.model)
        status = poll_until_terminal(
            base_url,
            token,
            request_id,
            poll_seconds=args.poll_seconds,
            poll_timeout=args.poll_timeout,
            answers=args.answer,
        )

        if status.get("status") != "succeeded":
            raise RuntimeError(f"Skill2API failed: {status.get('error', 'unknown error')}")
        download_results(base_url, token, request_id, status, args.output)
        return 0
    finally:
        if token:
            revoke_token(base_url, revoke_endpoint, args.client_id, token)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (KeyError, RuntimeError, urllib.error.URLError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        raise SystemExit(1)
