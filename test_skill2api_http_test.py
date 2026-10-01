"""Regression tests for the Skill2API HTTP smoke-test client."""

import json
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

import test_skill2api_http as client


class OAuthRefreshTests(unittest.TestCase):
    """Validate persisted OAuth refresh and retry behavior."""

    def make_auth(self, token_file: Path, *, expires_at: float | None) -> client.AuthenticatedClient:
        return client.AuthenticatedClient(
            token_file,
            "https://example.test",
            "client-1",
            client.OAuthToken(
                access_token="old-access",
                refresh_token="old-refresh",
                expires_at=expires_at,
                revoke_endpoint=None,
                token_endpoint="https://example.test/oauth/token",
            ),
        )

    def test_refreshes_before_access_token_expiry_and_persists_rotation(self) -> None:
        """Refresh expiring credentials before issuing the protected request."""
        with tempfile.TemporaryDirectory() as directory:
            token_file = Path(directory) / "token.json"
            auth = self.make_auth(token_file, expires_at=time.time() + 30)
            responses = iter([
                {"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600},
                {"status": "ok"},
            ])
            with patch.object(client, "request_json", side_effect=responses) as request_json:
                self.assertEqual({"status": "ok"}, auth.request_json("https://example.test/protected"))
            saved = json.loads(token_file.read_text(encoding="utf-8"))

        refresh_body = request_json.call_args_list[0].kwargs["body"].decode()
        self.assertIn("grant_type=refresh_token", refresh_body)
        self.assertIn("refresh_token=old-refresh", refresh_body)
        self.assertEqual("Bearer new-access", request_json.call_args_list[1].kwargs["headers"]["Authorization"])
        self.assertEqual("new-refresh", saved["refresh_token"])

    def test_retries_once_after_unauthorized_response(self) -> None:
        """Refresh an otherwise unexpired token once when the API rejects it."""
        with tempfile.TemporaryDirectory() as directory:
            auth = self.make_auth(Path(directory) / "token.json", expires_at=time.time() + 3600)
            responses = iter([
                client.HTTPError(401, "https://example.test/protected", "expired"),
                {"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600},
                {"status": "ok"},
            ])
            with patch.object(client, "request_json", side_effect=responses) as request_json:
                self.assertEqual({"status": "ok"}, auth.request_json("https://example.test/protected"))

        self.assertEqual(3, request_json.call_count)
        self.assertEqual("Bearer old-access", request_json.call_args_list[0].kwargs["headers"]["Authorization"])
        self.assertEqual("Bearer new-access", request_json.call_args_list[2].kwargs["headers"]["Authorization"])


class TemporaryDownloadURLTests(unittest.TestCase):
    """Validate asynchronous temporary-file delivery polling."""

    def test_submits_and_polls_until_delivery_succeeds(self) -> None:
        """Use the asynchronous delivery endpoints instead of the legacy GET."""
        responses = iter([
            {"request_id": "request-1", "delivery_id": "delivery-1", "status": "queued"},
            {"request_id": "request-1", "delivery_id": "delivery-1", "status": "queued"},
            {
                "request_id": "request-1",
                "delivery_id": "delivery-1",
                "status": "succeeded",
                "url": "/upload/ab/cd/abcdef.mp4",
            },
        ])
        with patch.object(client, "request_json", side_effect=lambda *args, **kwargs: next(responses)) as request_json, patch.object(client.time, "monotonic", side_effect=[0, 0]), patch.object(client.time, "sleep") as sleep:
            url = client.temporary_download_url(
                "https://example.test",
                "token",
                "request-1",
                "result.mp4",
                poll_seconds=2,
                poll_timeout=30,
            )

        self.assertEqual("/upload/ab/cd/abcdef.mp4", url)
        self.assertEqual(3, request_json.call_count)
        self.assertEqual("POST", request_json.call_args_list[0].kwargs["method"])
        self.assertIn("/api/skill2api/file/", request_json.call_args_list[0].args[0])
        self.assertIn("/api/skill2api/file/delivery/?", request_json.call_args_list[1].args[0])
        sleep.assert_called_once_with(2)

    def test_reports_async_delivery_failure(self) -> None:
        """Expose the worker's terminal delivery failure to the caller."""
        responses = iter([
            {"delivery_id": "delivery-1", "status": "queued"},
            {"status": "failed", "error": "upload timed out"},
        ])
        with patch.object(client, "request_json", side_effect=lambda *args, **kwargs: next(responses)):
            with self.assertRaisesRegex(RuntimeError, "upload timed out"):
                client.temporary_download_url(
                    "https://example.test", "token", "request-1", "result.mp4"
                )
