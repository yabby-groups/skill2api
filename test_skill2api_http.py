#!/usr/bin/env python3
"""End-to-end HTTP smoke test for Huabot Skill2API."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
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
TOKEN_REFRESH_LEEWAY_SECONDS = 60


class HTTPError(RuntimeError):
    """An HTTP failure whose status code can be handled by callers."""

    def __init__(self, code: int, url: str, detail: str) -> None:
        super().__init__(f"HTTP {code} {url}: {detail}")
        self.code = code


@dataclass
class OAuthToken:
    """OAuth credentials retained locally for one client and API origin."""

    access_token: str
    refresh_token: str | None
    expires_at: float | None
    revoke_endpoint: str | None
    token_endpoint: str | None


def save_token(
    token_file: Path,
    *,
    base_url: str,
    client_id: str,
    access_token: str,
    refresh_token: str | None,
    expires_at: float | None,
    revoke_endpoint: str | None,
    token_endpoint: str | None,
) -> None:
    """Atomically persist an OAuth token in an owner-readable-only file."""
    token_file.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    payload = {
        "base_url": base_url,
        "client_id": client_id,
        "access_token": access_token,
        "refresh_token": refresh_token,
        "expires_at": expires_at,
        "revoke_endpoint": revoke_endpoint,
        "token_endpoint": token_endpoint,
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


def load_token(token_file: Path, *, base_url: str, client_id: str) -> OAuthToken:
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
    def optional_string(name: str) -> str | None:
        value = payload.get(name)
        return value if isinstance(value, str) and value else None

    expires_at = payload.get("expires_at")
    return OAuthToken(
        access_token=token,
        refresh_token=optional_string("refresh_token"),
        expires_at=float(expires_at) if isinstance(expires_at, (int, float)) else None,
        revoke_endpoint=optional_string("revoke_endpoint"),
        token_endpoint=optional_string("token_endpoint"),
    )


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
        raise HTTPError(exc.code, url, detail) from exc
    try:
        value = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"Expected JSON from {url}") from exc
    if not isinstance(value, dict):
        raise RuntimeError(f"Expected JSON object from {url}")
    return value


def form_body(fields: dict[str, str]) -> bytes:
    return urllib.parse.urlencode(fields).encode("utf-8")


class AuthenticatedClient:
    """Refresh and persist OAuth credentials around authenticated requests."""

    def __init__(self, token_file: Path, base_url: str, client_id: str, token: OAuthToken) -> None:
        self.token_file = token_file
        self.base_url = base_url
        self.client_id = client_id
        self.token = token

    def save(self) -> None:
        save_token(
            self.token_file,
            base_url=self.base_url,
            client_id=self.client_id,
            access_token=self.token.access_token,
            refresh_token=self.token.refresh_token,
            expires_at=self.token.expires_at,
            revoke_endpoint=self.token.revoke_endpoint,
            token_endpoint=self.token.token_endpoint,
        )

    def refresh(self) -> None:
        if not self.token.refresh_token:
            raise RuntimeError("OAuth access token expired. Run with --login to authorize again.")
        response = request_json(
            self.token.token_endpoint or f"{self.base_url}/oauth/token",
            method="POST",
            headers={"Content-Type": "application/x-www-form-urlencoded"},
            body=form_body({
                "grant_type": "refresh_token",
                "client_id": self.client_id,
                "refresh_token": self.token.refresh_token,
            }),
        )
        access_token = response.get("access_token")
        if not isinstance(access_token, str) or not access_token:
            raise RuntimeError("OAuth token refresh returned no access token")
        rotated_refresh = response.get("refresh_token")
        if rotated_refresh is not None and (not isinstance(rotated_refresh, str) or not rotated_refresh):
            raise RuntimeError("OAuth token refresh returned an invalid refresh token")
        expires_in = response.get("expires_in")
        self.token.access_token = access_token
        if isinstance(rotated_refresh, str):
            self.token.refresh_token = rotated_refresh
        self.token.expires_at = time.time() + float(expires_in) if isinstance(expires_in, (int, float)) else None
        self.save()

    def ensure_fresh_token(self) -> None:
        if self.token.expires_at is not None and time.time() >= self.token.expires_at - TOKEN_REFRESH_LEEWAY_SECONDS:
            self.refresh()

    def request_json(self, url: str, **kwargs: Any) -> dict[str, Any]:
        self.ensure_fresh_token()
        headers = dict(kwargs.pop("headers", {}))
        headers["Authorization"] = f"Bearer {self.token.access_token}"
        try:
            return request_json(url, headers=headers, **kwargs)
        except HTTPError as exc:
            if exc.code != 401:
                raise
        self.refresh()
        retry_headers = dict(headers)
        retry_headers["Authorization"] = f"Bearer {self.token.access_token}"
        return request_json(url, headers=retry_headers, **kwargs)


def authenticated_request_json(
    auth: AuthenticatedClient | str, url: str, **kwargs: Any
) -> dict[str, Any]:
    """Make an authenticated request, preserving string-token helper compatibility."""
    if isinstance(auth, AuthenticatedClient):
        return auth.request_json(url, **kwargs)
    headers = dict(kwargs.pop("headers", {}))
    headers["Authorization"] = f"Bearer {auth}"
    return request_json(url, headers=headers, **kwargs)


def upload_file(base_url: str, auth: AuthenticatedClient | str, path: Path) -> dict[str, Any]:
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
    return authenticated_request_json(
        auth,
        f"{base_url}/api/file/run/",
        method="POST",
        headers={
            "Content-Type": f"multipart/form-data; boundary={boundary}",
        },
        body=b"".join(parts),
        timeout=180,
    )


def temporary_download_url(
    base_url: str,
    auth: AuthenticatedClient | str,
    request_id: str,
    path: str,
    *,
    poll_seconds: int = 5,
    poll_timeout: int = 1800,
) -> str:
    """Queue and await a worker-uploaded temporary download URL.

    Args:
        base_url: Myna HTTP API origin.
        token: OAuth access token for the owning Skill2API request.
        request_id: Generated Skill2API request identifier.
        path: Worker-approved relative output path.
        poll_seconds: Delay between delivery status requests.
        poll_timeout: Maximum time to wait for delivery completion.

    Returns:
        Relative temporary upload URL returned by the Skill2API file endpoint.

    Raises:
        RuntimeError: If the worker response has no safe temporary upload URL.
    """
    submitted = authenticated_request_json(
        auth,
        f"{base_url}/api/skill2api/file/",
        method="POST",
        headers={"Content-Type": "application/json"},
        body=json.dumps({"request_id": request_id, "file_path": path}).encode(),
    )
    delivery_id = submitted.get("delivery_id")
    if not isinstance(delivery_id, str) or not delivery_id:
        raise RuntimeError(f"Invalid temporary delivery submission for {path}")

    deadline = time.monotonic() + poll_timeout
    while True:
        query = urllib.parse.urlencode(
            {"request_id": request_id, "delivery_id": delivery_id}
        )
        payload = authenticated_request_json(
            auth,
            f"{base_url}/api/skill2api/file/delivery/?{query}",
        )
        state = payload.get("status")
        if state == "failed":
            error = payload.get("error")
            detail = error if isinstance(error, str) and error else "unknown error"
            raise RuntimeError(f"Temporary delivery failed for {path}: {detail}")
        if state == "succeeded":
            break
        if state not in {"queued", "running"}:
            raise RuntimeError(f"Invalid temporary delivery status for {path}: {state!r}")
        if time.monotonic() >= deadline:
            raise RuntimeError(f"Temporary delivery timed out for {path}")
        time.sleep(poll_seconds)

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
    auth: AuthenticatedClient | str,
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
    return authenticated_request_json(
        auth,
        f"{base_url}/api/skill2api/resume/",
        method="POST",
        headers={"Content-Type": "application/json"},
        body=json.dumps(payload).encode(),
    )


def terminate_request(base_url: str, auth: AuthenticatedClient | str, request_id: str) -> dict[str, Any]:
    """Terminate one existing Skill2API request."""
    return authenticated_request_json(
        auth,
        f"{base_url}/api/skill2api/terminate/",
        method="POST",
        headers={"Content-Type": "application/json"},
        body=json.dumps({"request_id": request_id}).encode(),
    )


def get_status(base_url: str, auth: AuthenticatedClient | str, request_id: str) -> dict[str, Any]:
    """Fetch the public status for one Skill2API request."""
    query = urllib.parse.urlencode({"request_id": request_id})
    return authenticated_request_json(
        auth,
        f"{base_url}/api/skill2api/status/?{query}",
    )


def answer_pending_request(
    base_url: str,
    auth: AuthenticatedClient | str,
    request_id: str,
    status: dict[str, Any],
) -> None:
    """Interactively answer one pending Skill2API clarification."""
    answer = choose_answer(status.get("question"), status.get("options"))
    resumed = resume_request(base_url, auth, request_id, answer=answer)
    print(f"resume status={resumed.get('status')}")


def poll_until_terminal(
    base_url: str,
    auth: AuthenticatedClient | str,
    request_id: str,
    *,
    poll_seconds: int,
    poll_timeout: int,
) -> dict[str, Any]:
    """Poll a request, interactively resuming each pending clarification."""
    deadline = time.monotonic() + poll_timeout
    terminal = {"succeeded", "failed", "terminated"}
    while True:
        status = get_status(base_url, auth, request_id)
        state = status.get("status")
        print(f"status={state}")
        if state == "waiting_for_input":
            answer_pending_request(base_url, auth, request_id, status)
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


def poll_device_token(device: dict[str, Any], token_endpoint: str, client_id: str) -> dict[str, Any]:
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
        if not isinstance(response.get("access_token"), str) or not response["access_token"]:
            raise RuntimeError("OAuth response returned no access token")
        print(f"OAuth authorized; scope={response.get('scope', '')}")
        return response
    raise RuntimeError("OAuth authorization timed out")


def authorize_device(base_url: str, client_id: str) -> OAuthToken:
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
    response = poll_device_token(device, metadata["token_endpoint"], client_id)
    expires_in = response.get("expires_in")
    return OAuthToken(
        access_token=response["access_token"],
        refresh_token=response.get("refresh_token") if isinstance(response.get("refresh_token"), str) else None,
        expires_at=time.time() + float(expires_in) if isinstance(expires_in, (int, float)) else None,
        revoke_endpoint=metadata.get("revocation_endpoint") if isinstance(metadata.get("revocation_endpoint"), str) else None,
        token_endpoint=metadata["token_endpoint"],
    )


def uploaded_file_url(base_url: str, upload: dict[str, Any]) -> str:
    """Turn a temporary upload response into its public provider URL."""
    file = upload["file"]
    file_key = str(file["file_key"])
    file_ext = str(file["file_ext"])
    shard = get_shard_relative_path(file_key)
    return f"{base_url}/upload/{shard}/{file_key}.{file_ext}"


def upload_source_media(base_url: str, auth: AuthenticatedClient | str, image: Path, video: Path) -> tuple[str, str]:
    """Upload image and video sources, returning their temporary provider URLs."""
    image_upload = upload_file(base_url, auth, image)
    video_upload = upload_file(base_url, auth, video)
    image_url = uploaded_file_url(base_url, image_upload)
    video_url = uploaded_file_url(base_url, video_upload)
    print(f"Uploaded temporary image id={image_upload['file'].get('id')} {image_url}")
    print(f"Uploaded temporary video id={video_upload['file'].get('id')} {video_url}")
    return image_url, video_url


def build_hypit_prompt(video_url: str, image_url: str) -> str:
    """Build the requested Hypit clone instruction from uploaded source URLs."""
    return f"""
$hypit 克隆视频 {video_url} 替换商品为 {image_url} 替换人物为 公开的虚拟人像: asset://asset-20260720205609-dvhxr

视频里面有人脸，你不能通过编辑的方式制作视频

尺寸 480p  9:16

预算 2 美金

"""


def submit_generation(base_url: str, auth: AuthenticatedClient | str, prompt: str, model: str) -> str:
    """Create the Hypit Skill2API request and return its server-generated ID."""
    generated = authenticated_request_json(
        auth,
        f"{base_url}/api/skill2api/generate/",
        method="POST",
        headers={"Content-Type": "application/json"},
        body=json.dumps({"prompt": prompt, "skill_name": "hypit", "model": model}).encode(),
    )
    request_id = str(generated["request_id"])
    print(f"Submitted request_id={request_id}, model={model}")
    return request_id


def final_mp4_from_status(status: dict[str, Any]) -> str | None:
    """Return the final task MP4 linked from status logs, with a files fallback.

    Codex can write its terminal response to either stderr or stdout. A link is
    accepted only when it names an MP4 listed in the worker's task-local files.
    """
    files = status.get("files") or []
    available = {path for path in files if isinstance(path, str)}

    for stream_name in ("stderr", "stdout"):
        stream = status.get(stream_name)
        if not isinstance(stream, str):
            continue
        matches = list(MARKDOWN_LINK_RE.finditer(stream))
        for match in reversed(matches):
            target = urllib.parse.unquote(match.group(1).strip())
            parsed = urllib.parse.urlsplit(target)
            if parsed.scheme or parsed.netloc or parsed.query or parsed.fragment:
                continue
            path = parsed.path
            if path.startswith("/workspace/"):
                path = path.removeprefix("/workspace/")
            elif path.startswith("/"):
                continue
            path = posixpath.normpath(path)
            if path in {".", ".."} or path.startswith("../"):
                continue
            if path.lower().endswith(".mp4") and path in available:
                return path

    return next(
        (
            relative_path
            for relative_path in reversed(files)
            if isinstance(relative_path, str) and relative_path.lower().endswith(".mp4")
        ),
        None,
    )


def download_results(
    base_url: str,
    auth: AuthenticatedClient | str,
    request_id: str,
    status: dict[str, Any],
    output: Path,
    *,
    poll_seconds: int,
    poll_timeout: int,
) -> None:
    """Download the final MP4 reported by the successful status response."""
    output.mkdir(parents=True, exist_ok=True)
    final_mp4 = final_mp4_from_status(status)
    if final_mp4 is None:
        raise RuntimeError("Skill2API succeeded but status reported no MP4 result")

    print(final_mp4)
    destination = output / final_mp4
    destination.parent.mkdir(parents=True, exist_ok=True)
    url = temporary_download_url(
        base_url,
        auth,
        request_id,
        final_mp4,
        poll_seconds=poll_seconds,
        poll_timeout=poll_timeout,
    )
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
        token = authorize_device(base_url, args.client_id)
        save_token(
            args.token_file,
            base_url=base_url,
            client_id=args.client_id,
            access_token=token.access_token,
            refresh_token=token.refresh_token,
            expires_at=token.expires_at,
            revoke_endpoint=token.revoke_endpoint,
            token_endpoint=token.token_endpoint,
        )
        print(f"Login saved to {args.token_file}")
        return 0
    if args.logout:
        token = load_token(
            args.token_file, base_url=base_url, client_id=args.client_id
        )
        try:
            revoke_token(base_url, token.revoke_endpoint, args.client_id, token.refresh_token or token.access_token)
        finally:
            remove_token(args.token_file)
        print("Logged out")
        return 0

    auth = AuthenticatedClient(
        args.token_file,
        base_url,
        args.client_id,
        load_token(args.token_file, base_url=base_url, client_id=args.client_id),
    )
    if args.download_video:
        request_id = args.download_video
        status = get_status(base_url, auth, request_id)
        if status.get("status") != "succeeded":
            raise RuntimeError(
                "Skill2API request is not ready to download: "
                f"{status.get('error', status.get('status'))}"
            )
        download_results(
            base_url,
            auth,
            request_id,
            status,
            args.output,
            poll_seconds=args.poll_seconds,
            poll_timeout=args.poll_timeout,
        )
        return 0
    if args.request_id:
        request_id = args.request_id
        if args.terminate:
            terminated = terminate_request(base_url, auth, request_id)
            print(f"terminate status={terminated.get('status')}")
            return 0
        resumed = resume_request(
            base_url,
            auth,
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
        image_url, video_url = upload_source_media(base_url, auth, args.image, args.video)
        prompt = build_hypit_prompt(video_url, image_url)
        print(prompt)
        request_id = submit_generation(base_url, auth, prompt, args.model)
    status = poll_until_terminal(
        base_url,
        auth,
        request_id,
        poll_seconds=args.poll_seconds,
        poll_timeout=args.poll_timeout,
    )

    if status.get("status") != "succeeded":
        raise RuntimeError(f"Skill2API failed: {status.get('error', 'unknown error')}")
    download_results(
        base_url,
        auth,
        request_id,
        status,
        args.output,
        poll_seconds=args.poll_seconds,
        poll_timeout=args.poll_timeout,
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (KeyError, RuntimeError, urllib.error.URLError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        raise SystemExit(1)
