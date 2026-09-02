#!/usr/bin/env python3

from __future__ import annotations

from contextlib import nullcontext
import importlib.machinery
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest import mock


HELPER = Path(__file__).resolve().parents[1] / "scripts" / "codex-task-control"


def load_helper():
    loader = importlib.machinery.SourceFileLoader("codex_task_control_test", str(HELPER))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    if spec is None:
        raise RuntimeError("could not load codex-task-control")
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


control = load_helper()


class FakeClient:
    def __init__(self, responses, notification=None):
        self.responses = responses
        self.notification = notification
        self.requests = []
        self.server_user_agent = "test-server"

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return None

    def request(self, method, params):
        self.requests.append((method, params))
        response = self.responses[method]
        return response(params) if callable(response) else response

    def take_pending_notification(self, method, predicate):
        if self.notification is not None and predicate(self.notification):
            return self.notification
        return None


class CodexTaskControlTest(unittest.TestCase):
    def test_inspect_preserves_removed_absolute_thread_cwd(self):
        with tempfile.TemporaryDirectory(prefix="codex-task-control-") as parent:
            removed_cwd = Path(parent) / "removed-worktree"
            client = FakeClient(
                {
                    "thread/read": {
                        "thread": {
                            "id": "thread-1",
                            "cwd": str(removed_cwd),
                            "name": "ws/feat/example",
                            "ephemeral": False,
                            "status": {"type": "active"},
                            "turns": [],
                        }
                    }
                }
            )

            with mock.patch.object(control, "AppServerClient", return_value=client):
                result = control.inspect_thread(
                    Path("/socket"), 1.0, "thread-1"
                )

        self.assertEqual(str(removed_cwd.resolve(strict=False)), result["cwd"])
        self.assertTrue(result["verified"])

    def test_repeated_mode_accepts_successful_noop_without_waiting(self):
        client = FakeClient(
            {
                "thread/resume": {"thread": {"id": "thread-1"}, "model": "model-1"},
                "collaborationMode/list": {
                    "data": [
                        {
                            "mode": "plan",
                            "model": "model-1",
                            "reasoning_effort": "medium",
                        }
                    ]
                },
                "thread/settings/update": {},
            }
        )

        with mock.patch.object(control, "AppServerClient", return_value=client):
            result = control.set_mode(Path("/socket"), 1.0, "thread-1", "plan")

        self.assertTrue(result["verified"])
        self.assertEqual("requestAcknowledged", result["verification"])
        self.assertEqual("plan", result["mode"])

    def test_mode_validates_observed_notification(self):
        notification = {
            "method": "thread/settings/updated",
            "params": {
                "threadId": "thread-1",
                "threadSettings": {
                    "model": "model-1",
                    "effort": "high",
                    "collaborationMode": {"mode": "plan"},
                },
            },
        }
        client = FakeClient(
            {
                "thread/resume": {"thread": {"id": "thread-1"}, "model": "model-1"},
                "collaborationMode/list": {
                    "data": [
                        {
                            "mode": "plan",
                            "model": "model-1",
                            "reasoning_effort": "high",
                        }
                    ]
                },
                "thread/settings/update": {},
            },
            notification=notification,
        )

        with mock.patch.object(control, "AppServerClient", return_value=client):
            result = control.set_mode(Path("/socket"), 1.0, "thread-1", "plan")

        self.assertEqual("notification", result["verification"])
        self.assertEqual("high", result["reasoningEffort"])

    def test_completed_turn_prevents_duplicate_queue(self):
        prompt = "exact prompt"
        client = FakeClient(
            {
                "thread/read": {
                    "thread": {
                        "id": "thread-1",
                        "turns": [
                            {
                                "id": "turn-1",
                                "status": "completed",
                                "items": [
                                    {
                                        "type": "userMessage",
                                        "content": [{"type": "text", "text": prompt}],
                                    }
                                ],
                            }
                        ],
                    }
                }
            }
        )

        with mock.patch.object(control, "AppServerClient", return_value=client):
            with mock.patch.object(
                control,
                "find_queued_message",
                side_effect=AssertionError("queue should not be inspected"),
            ):
                result = control.queue_message(Path("/socket"), 1.0, "thread-1", prompt)

        self.assertTrue(result["reused"])
        self.assertEqual("turnHistory", result["source"])
        self.assertEqual("turn-1", result["turnId"])

    def test_pending_only_queue_ignores_history_and_requires_real_submission(self):
        prompt = "$task finish"
        completed = mock.Mock(returncode=0, stdout="", stderr="")

        with mock.patch.object(
            control,
            "find_submitted_message",
            side_effect=AssertionError("turn history must not be inspected"),
        ):
            with mock.patch.object(
                control,
                "find_queued_message",
                side_effect=(None, {"id": "submission-1"}),
            ):
                with mock.patch.object(control.shutil, "which", return_value="/codex"):
                    with mock.patch.object(
                        control.subprocess, "run", return_value=completed
                    ) as run:
                        result = control.queue_message(
                            Path("/socket"),
                            1.0,
                            "thread-1",
                            prompt,
                            pending_only=True,
                        )

        self.assertTrue(result["queued"])
        self.assertFalse(result["reused"])
        self.assertEqual("pending", result["state"])
        self.assertEqual("submission-1", result["queuedSubmissionId"])
        self.assertNotIn("source", result)
        run.assert_called_once()

    def test_pending_only_queue_rejects_unverified_acceptance(self):
        completed = mock.Mock(returncode=0, stdout="", stderr="")

        with mock.patch.object(control, "find_queued_message", return_value=None):
            with mock.patch.object(control.shutil, "which", return_value="/codex"):
                with mock.patch.object(control.subprocess, "run", return_value=completed):
                    with self.assertRaises(control.ControlError):
                        control.queue_message(
                            Path("/socket"),
                            1.0,
                            "thread-1",
                            "$task finish",
                            pending_only=True,
                        )

    def test_queue_parser_exposes_pending_only_mode(self):
        args = control.build_parser().parse_args(
            [
                "--thread",
                "thread-1",
                "queue",
                "--pending-only",
                "$task finish",
            ]
        )

        self.assertTrue(args.pending_only)

    def test_ensure_thread_creates_and_verifies_an_empty_named_thread(self):
        with tempfile.TemporaryDirectory(prefix="codex-task-control-") as raw_cwd:
            cwd = Path(raw_cwd).resolve()
            name = "ws/feat/example"
            created = {
                "id": "thread-new",
                "cwd": str(cwd),
                "name": name,
                "ephemeral": False,
                "status": {"type": "idle"},
                "turns": [],
            }
            client = FakeClient(
                {
                    "thread/list": {"data": [], "nextCursor": None},
                    "thread/start": {"thread": {"id": "thread-new"}},
                    "thread/name/set": {},
                    "thread/read": {"thread": created},
                }
            )

            with mock.patch.object(control, "AppServerClient", return_value=client):
                with mock.patch.object(
                    control,
                    "thread_ensure_lock",
                    return_value=nullcontext(),
                ):
                    result = control.ensure_thread(
                        Path("/socket"),
                        1.0,
                        cwd,
                        name,
                        None,
                        "model-1",
                        "on-request",
                        "auto_review",
                        "workspace-write",
                    )

        self.assertFalse(result["reused"])
        self.assertTrue(result["empty"])
        self.assertTrue(result["verified"])
        start = next(params for method, params in client.requests if method == "thread/start")
        self.assertEqual(str(cwd), start["cwd"])
        self.assertFalse(start["ephemeral"])
        self.assertEqual("workspace-write", start["sandbox"])


if __name__ == "__main__":
    unittest.main()
