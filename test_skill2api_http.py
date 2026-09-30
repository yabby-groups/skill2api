#!/usr/bin/env python3
"""End-to-end HTTP smoke test for Huabot Skill2API."""

from __future__ import annotations

import argparse
import json
import mimetypes
import os
import posixpath
import re
import stat
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser
from pathlib import Path
from typing import Any


DEFAULT_BASE_URL = "https://huabot.com"
DEFAULT_CLIENT_ID = "kRP-pREgGsmqLNlFeWKiJDGmS7JbSJob"
SCOPES = "files:upload skill2api offline_access"
DEFAULT_TOKEN_FILE = (
    Path(os.environ.get("XDG_STATE_HOME", Path.home() / ".local" / "state"))
    / "skill2api-http"
    / "token.json"
)
MARKDOWN_LINK_RE = re.compile(r"\[[^\]]*\]\(\s*([^)]*?)\s*\)")


def save_token(
    token_file: Path,
    *,
    base_url: str,
    client_id: str,
    access_token: str,
    revoke_endpoint: str | None,
) -> None:
    """Atomically persist an OAuth token in an owner-readable-only file."""
    token_file.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    payload = {
        "base_url": base_url,
        "client_id": client_id,
        "access_token": access_token,
        "revoke_endpoint": revoke_endpoint,
    }
    descriptor, temporary_name = tempfile.mkstemp(prefix=".token-", dir=token_file.parent)
    temporary_file = Path(temporary_name)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(payload, handle)
            handle.write("\n")
        os.replace(temporary_file, token_file)
    finally:
        if temporary_file.exists():
            temporary_file.unlink()


def load_token(token_file: Path, *, base_url: str, client_id: str) -> tuple[str, str | None]:
    """Load a token only when it belongs to the requested OAuth client."""
    if not token_file.is_file() or token_file.is_symlink():
        raise RuntimeError("Not logged in. Run with --login first.")
    if stat.S_IMODE(token_file.stat().st_mode) & 0o077:
        raise RuntimeError(f"Token file has unsafe permissions: {token_file}")
    try:
        payload = json.loads(token_file.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise RuntimeError(f"Could not read token file: {token_file}") from exc
    if not isinstance(payload, dict):
        raise RuntimeError(f"Invalid token file: {token_file}")
    if payload.get("base_url") != base_url or payload.get("client_id") != client_id:
        raise RuntimeError("Saved token is for a different base URL or client ID. Run with --login.")
    token = payload.get("access_token")
    if not isinstance(token, str) or not token:
        raise RuntimeError(f"Invalid token file: {token_file}")
    revoke_endpoint = payload.get("revoke_endpoint")
    return token, revoke_endpoint if isinstance(revoke_endpoint, str) else None


def remove_token(token_file: Path) -> None:
    """Remove the local token after logout without touching unrelated files."""
    if token_file.is_file() and not token_file.is_symlink():
        token_file.unlink()


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


def resume_request(
    base_url: str,
    token: str,
    request_id: str,
    *,
    answer: str | None = None,
    instruction: str | None = None,
) -> dict[str, Any]:
    """Resume one existing request with either an answer or an instruction."""
    if bool(answer) == bool(instruction):
        raise ValueError("provide exactly one of answer or instruction")
    payload: dict[str, str] = {"request_id": request_id}
    if answer:
        payload["answer"] = answer
    else:
        payload["instruction"] = instruction or ""
    return request_json(
        f"{base_url}/api/skill2api/resume/",
        method="POST",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        body=json.dumps(payload).encode(),
    )


def terminate_request(base_url: str, token: str, request_id: str) -> dict[str, Any]:
    """Terminate one existing Skill2API request."""
    return request_json(
        f"{base_url}/api/skill2api/terminate/",
        method="POST",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        body=json.dumps({"request_id": request_id}).encode(),
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
) -> None:
    """Interactively answer one pending Skill2API clarification."""
    answer = choose_answer(status.get("question"), status.get("options"))
    resumed = resume_request(base_url, token, request_id, answer=answer)
    print(f"resume status={resumed.get('status')}")


def poll_until_terminal(
    base_url: str,
    token: str,
    request_id: str,
    *,
    poll_seconds: int,
    poll_timeout: int,
) -> dict[str, Any]:
    """Poll a request, interactively resuming each pending clarification."""
    deadline = time.monotonic() + poll_timeout
    terminal = {"succeeded", "failed", "terminated"}
    while True:
        status = get_status(base_url, token, request_id)
        state = status.get("status")
        print(f"status={state}")
        if state == "waiting_for_input":
            answer_pending_request(base_url, token, request_id, status)
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
    parser.add_argument("video", nargs="?", type=Path)
    parser.add_argument("image", nargs="?", type=Path)
    parser.add_argument("--base-url", default=DEFAULT_BASE_URL)
    parser.add_argument("--client-id", default=DEFAULT_CLIENT_ID)
    parser.add_argument("--model", default="qwen3.8-flash")
    parser.add_argument("--output", type=Path, default=Path("skill2api-result"))
    parser.add_argument("--token-file", type=Path, default=DEFAULT_TOKEN_FILE)
    parser.add_argument("--poll-seconds", type=int, default=5)
    parser.add_argument("--poll-timeout", type=int, default=1800)
    authentication = parser.add_mutually_exclusive_group()
    authentication.add_argument(
        "--login",
        action="store_true",
        help="authorize in the browser and save the access token",
    )
    authentication.add_argument(
        "--logout",
        action="store_true",
        help="revoke and remove the saved access token",
    )
    parser.add_argument("--request-id", help="resume this existing Skill2API request")
    parser.add_argument(
        "--download-video",
        metavar="REQUEST_ID",
        help="download the final MP4 from an already-succeeded Skill2API request",
    )
    continuation = parser.add_mutually_exclusive_group()
    continuation.add_argument(
        "--answer",
        metavar="ANSWER",
        help="answer a pending question for --request-id",
    )
    continuation.add_argument(
        "--append-prompt",
        metavar="TEXT",
        help="append an instruction to a resumable --request-id task",
    )
    continuation.add_argument(
        "--terminate",
        action="store_true",
        help="terminate the --request-id task without resuming it",
    )
    args = parser.parse_args()
    if args.login or args.logout:
        if any(
            (
                args.video,
                args.image,
                args.request_id,
                args.download_video,
                args.answer,
                args.append_prompt,
                args.terminate,
            )
        ):
            parser.error("--login and --logout cannot be combined with task options")
        return args
    if args.request_id and args.download_video:
        parser.error("--request-id and --download-video cannot be used together")
    if args.download_video:
        if args.answer or args.append_prompt or args.terminate:
            parser.error("--answer, --append-prompt, and --terminate require --request-id")
        if args.video or args.image:
            parser.error("video and image are only valid when creating a new request")
    elif args.request_id:
        if not args.answer and not args.append_prompt and not args.terminate:
            parser.error("--request-id requires --answer, --append-prompt, or --terminate")
        if args.video or args.image:
            parser.error("video and image are only valid when creating a new request")
    else:
        if args.answer or args.append_prompt or args.terminate:
            parser.error("--answer, --append-prompt, and --terminate require --request-id")
        if not args.video or not args.image:
            parser.error("video and image are required when creating a new request")
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
    if args.login:
        token, revoke_endpoint = authorize_device(base_url, args.client_id)
        save_token(
            args.token_file,
            base_url=base_url,
            client_id=args.client_id,
            access_token=token,
            revoke_endpoint=revoke_endpoint,
        )
        print(f"Login saved to {args.token_file}")
        return 0
    if args.logout:
        token, revoke_endpoint = load_token(
            args.token_file, base_url=base_url, client_id=args.client_id
        )
        try:
            revoke_token(base_url, revoke_endpoint, args.client_id, token)
        finally:
            remove_token(args.token_file)
        print("Logged out")
        return 0

    token, _ = load_token(args.token_file, base_url=base_url, client_id=args.client_id)
    if args.download_video:
        request_id = args.download_video
        status = get_status(base_url, token, request_id)
        if status.get("status") != "succeeded":
            raise RuntimeError(
                "Skill2API request is not ready to download: "
                f"{status.get('error', status.get('status'))}"
            )
        download_results(base_url, token, request_id, status, args.output)
        return 0
    if args.request_id:
        request_id = args.request_id
        if args.terminate:
            terminated = terminate_request(base_url, token, request_id)
            print(f"terminate status={terminated.get('status')}")
            return 0
        resumed = resume_request(
            base_url,
            token,
            request_id,
            answer=args.answer,
            instruction=args.append_prompt,
        )
        if resumed.get("status") != "running":
            raise RuntimeError(
                f"Skill2API resume failed: {resumed.get('error', resumed.get('status'))}"
            )
        print(f"Resumed request_id={request_id}")
    else:
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
    )

    if status.get("status") != "succeeded":
        raise RuntimeError(f"Skill2API failed: {status.get('error', 'unknown error')}")
    download_results(base_url, token, request_id, status, args.output)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (KeyError, RuntimeError, urllib.error.URLError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        raise SystemExit(1)
