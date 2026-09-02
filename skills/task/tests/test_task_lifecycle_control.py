#!/usr/bin/env python3

from __future__ import annotations

import importlib.machinery
import importlib.util
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


HELPER = Path(__file__).resolve().parents[1] / "scripts" / "task-lifecycle-control"


def load_helper():
    name = "task_lifecycle_control_test"
    loader = importlib.machinery.SourceFileLoader(name, str(HELPER))
    spec = importlib.util.spec_from_loader(name, loader)
    if spec is None:
        raise RuntimeError("could not load task-lifecycle-control")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    loader.exec_module(module)
    return module


lifecycle = load_helper()


class GitFixture:
    def __init__(self, repo_name: str = "demo") -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="task-lifecycle-")
        self.workspace = Path(self.temp.name) / "workspace"
        self.repo = self.workspace / repo_name / repo_name
        self.repo.mkdir(parents=True)
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.email", "test@example.com")
        self.git("config", "user.name", "Task Lifecycle Test")
        (self.repo / "README.md").write_text("base\n")
        self.git("add", "README.md")
        self.git("commit", "-qm", "base")

    def close(self) -> None:
        self.temp.cleanup()

    def git(
        self, *args: str, check: bool = True
    ) -> subprocess.CompletedProcess[str]:
        return self.git_at(self.repo, *args, check=check)

    def git_at(
        self, checkout: Path, *args: str, check: bool = True
    ) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ("git", "-C", str(checkout), *args),
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=check,
        )

    def commit_file(
        self,
        checkout: Path,
        relative: str,
        content: str,
        message: str,
    ) -> str:
        path = checkout / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
        self.git_at(checkout, "add", relative)
        self.git_at(checkout, "commit", "-qm", message)
        return self.git_at(checkout, "rev-parse", "HEAD").stdout.strip()

    def add_local_origin(self) -> Path:
        remote = Path(self.temp.name) / "origin.git"
        subprocess.run(
            ("git", "init", "--bare", "-q", "--initial-branch=main", str(remote)),
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=True,
        )
        self.git("remote", "add", "origin", str(remote))
        self.git("push", "-qu", "origin", "main")
        return remote

    def worktree(self, branch: str = "ws/feat/example") -> Path:
        runner = lifecycle.CommandRunner()
        stable, common = lifecycle.validate_stable_repo(runner, str(self.repo), self.workspace)
        result = lifecycle.ensure_worktree(
            runner,
            stable,
            common,
            self.workspace,
            branch,
            "main",
        )
        return Path(result["path"])


class TaskLifecycleControlTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fixture = GitFixture()

    def tearDown(self) -> None:
        self.fixture.close()

    def test_default_cc_data_dir_uses_cc_connect_state_subdirectory(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(
                Path.home() / ".cc-connect" / "data",
                lifecycle.configured_cc_data_dir(),
            )

        with mock.patch.dict(
            os.environ,
            {"CC_CONNECT_TASK_DATA_DIR": "~/custom-cc-data"},
            clear=True,
        ):
            self.assertEqual(
                Path.home() / "custom-cc-data",
                lifecycle.configured_cc_data_dir(),
            )

    def stable_finish_args(self, **overrides):
        cc_data_dir = self.fixture.workspace / "cc-data"
        cc_data_dir.mkdir(exist_ok=True)
        values = {
            "workspace_root": str(self.fixture.workspace),
            "repo": str(self.fixture.repo),
            "branch": None,
            "stable_checkout": True,
            "group_name": None,
            "project": "workspace",
            "record_url": None,
            "document_ready": False,
            "document_finalized": False,
            "thread_id": None,
            "codex_control": str(
                Path(__file__).resolve().parents[1]
                / "scripts"
                / "codex-task-control"
            ),
            "codex_socket": None,
            "cc_connect": "cc-connect",
            "cc_data_dir": str(cc_data_dir),
            "lark_cli": "lark-cli",
        }
        values.update(overrides)
        return type("Args", (), values)()

    def write_cc_attachment(
        self,
        thread_id: str,
        *,
        session_key: str = "feishu:oc_stable",
        internal_id: str = "s3",
    ) -> Path:
        sessions_dir = self.fixture.workspace / "cc-data" / "sessions"
        sessions_dir.mkdir(parents=True, exist_ok=True)
        path = sessions_dir / "workspace_ws_stable.json"
        path.write_text(
            json.dumps(
                {
                    "sessions": {
                        internal_id: {
                            "id": internal_id,
                            "name": "stable task",
                            "agent_session_id": thread_id,
                        }
                    },
                    "active_session": {session_key: internal_id},
                    "user_sessions": {session_key: [internal_id]},
                    "user_meta": {
                        session_key: {"chat_name": "demo-stable-task"}
                    },
                }
            ),
            encoding="utf-8",
        )
        return path

    def test_json_parser_tolerates_progress_lines(self) -> None:
        payload = lifecycle.parse_json_output(
            "[page 1] fetching...\nFound 3 members\n{\"ok\":true,\"data\":{}}\n",
            "test",
        )
        self.assertTrue(payload["ok"])

    def test_lark_null_collection_means_empty_page(self) -> None:
        class FakeRunner:
            def json(self, args, **_kwargs):
                payload = {
                    "ok": True,
                    "data": {"chats": None, "has_more": False, "page_token": ""},
                }
                command = tuple(str(item) for item in args)
                return lifecycle.CompletedCommand(command, 0, "{}", ""), payload

        self.assertEqual(
            [],
            lifecycle.search_groups(
                FakeRunner(), "lark-cli", "missing-group", "bot"
            ),
        )

    def test_group_create_sets_native_session_id_description(self) -> None:
        class FakeRunner:
            def __init__(self) -> None:
                self.calls = []

            def json(self, args, **_kwargs):
                command = tuple(str(item) for item in args)
                self.calls.append(command)
                payload = {
                    "ok": True,
                    "data": {
                        "chat_id": "oc_test",
                        "share_link": "https://example.test/share",
                    },
                }
                return lifecycle.CompletedCommand(command, 0, "{}", ""), payload

        runner = FakeRunner()
        result = lifecycle.create_group(
            runner, "lark-cli", "demo-task", "thread-exact"
        )

        self.assertEqual("thread-exact", result["description"])
        self.assertEqual("--description", runner.calls[0][5])
        self.assertEqual("thread-exact", runner.calls[0][6])
        self.assertIn("--as", runner.calls[0])
        self.assertEqual("bot", runner.calls[0][runner.calls[0].index("--as") + 1])

    def test_group_description_reuses_matching_session_id(self) -> None:
        class FakeRunner:
            def __init__(self) -> None:
                self.calls = []

            def json(self, args, **_kwargs):
                command = tuple(str(item) for item in args)
                self.calls.append(command)
                payload = {"ok": True, "data": {"description": "thread-exact"}}
                return lifecycle.CompletedCommand(command, 0, "{}", ""), payload

        runner = FakeRunner()
        result = lifecycle.ensure_group_session_description(
            runner, "lark-cli", "oc_test", "thread-exact"
        )

        self.assertEqual(
            {"value": "thread-exact", "updated": False, "verified": True},
            result,
        )
        self.assertEqual(
            (
                "lark-cli",
                "im",
                "chats",
                "get",
                "--chat-id",
                "oc_test",
                "--as",
                "bot",
                "--format",
                "json",
            ),
            runner.calls[0],
        )
        self.assertEqual(1, len(runner.calls))

    def test_group_description_repairs_and_verifies_session_id(self) -> None:
        class FakeRunner:
            def __init__(self) -> None:
                self.calls = []
                self.responses = iter(
                    (
                        {"ok": True, "data": {"description": "thread-old"}},
                        {"ok": True, "data": {}},
                        {"ok": True, "data": {"description": "thread-exact"}},
                    )
                )

            def json(self, args, **_kwargs):
                command = tuple(str(item) for item in args)
                self.calls.append(command)
                return (
                    lifecycle.CompletedCommand(command, 0, "{}", ""),
                    next(self.responses),
                )

        runner = FakeRunner()
        result = lifecycle.ensure_group_session_description(
            runner, "lark-cli", "oc_test", "thread-exact"
        )

        self.assertTrue(result["updated"])
        self.assertEqual(
            (
                "lark-cli",
                "im",
                "+chat-update",
                "--chat-id",
                "oc_test",
                "--description",
                "thread-exact",
                "--as",
                "bot",
                "--format",
                "json",
            ),
            runner.calls[1],
        )
        self.assertEqual(3, len(runner.calls))

    def test_group_description_rejects_unverified_update(self) -> None:
        class FakeRunner:
            def __init__(self) -> None:
                self.responses = iter(
                    (
                        {"ok": True, "data": {"description": "thread-old"}},
                        {"ok": True, "data": {}},
                        {"ok": True, "data": {"description": "thread-other"}},
                    )
                )

            def json(self, args, **_kwargs):
                command = tuple(str(item) for item in args)
                return (
                    lifecycle.CompletedCommand(command, 0, "{}", ""),
                    next(self.responses),
                )

        with self.assertRaises(lifecycle.LifecycleError) as caught:
            lifecycle.ensure_group_session_description(
                FakeRunner(), "lark-cli", "oc_test", "thread-exact"
            )

        self.assertEqual("group_description_conflict", caught.exception.code)
        self.assertEqual("thread-other", caught.exception.details["actual"])

    def test_task_group_create_and_description_share_one_session_id(self) -> None:
        group = {
            "chatId": "oc_test",
            "name": "demo-task",
            "createdByLifecycle": True,
        }
        description = {
            "value": "thread-exact",
            "updated": False,
            "verified": True,
        }

        with mock.patch.object(lifecycle, "discover_group", return_value=None):
            with mock.patch.object(
                lifecycle, "create_group", return_value=group.copy()
            ) as create:
                with mock.patch.object(
                    lifecycle,
                    "verify_group_members",
                    return_value={"verified": True},
                ):
                    with mock.patch.object(
                        lifecycle,
                        "ensure_group_session_description",
                        return_value=description,
                    ) as ensure_description:
                        result = lifecycle.ensure_task_group(
                            lifecycle.CommandRunner(),
                            "lark-cli",
                            "demo-task",
                            "thread-exact",
                        )

        create.assert_called_once_with(
            mock.ANY, "lark-cli", "demo-task", "thread-exact"
        )
        ensure_description.assert_called_once_with(
            mock.ANY, "lark-cli", "oc_test", "thread-exact"
        )
        self.assertEqual(description, result["sessionDescription"])

    def test_cc_unbind_uses_exact_worktree_and_native_session_cas(self) -> None:
        class FakeRunner:
            def __init__(self):
                self.args = None

            def json(self, args, **_kwargs):
                self.args = tuple(str(item) for item in args)
                payload = {
                    "schema_version": 1,
                    "ok": True,
                    "result": {
                        "project": "workspace",
                        "session": "feishu:oc_test",
                        "status": "unbound",
                    },
                }
                return lifecycle.CompletedCommand(self.args, 0, "{}", ""), payload

        runner = FakeRunner()
        result = lifecycle.cc_control(
            runner,
            "cc-connect",
            "workspace",
            "unbind",
            project="workspace",
            session_key="feishu:oc_test",
            data_dir=Path("/data"),
            worktree=Path("/workspace/task"),
            expected_agent_session_id="thread-exact",
        )

        self.assertTrue(result["ok"])
        self.assertIn("--worktree", runner.args)
        self.assertIn("/workspace/task", runner.args)
        self.assertIn("--expected-agent-session-id", runner.args)
        self.assertIn("thread-exact", runner.args)
        self.assertIn("--json", runner.args)
        self.assertEqual("workspace", result["project"])
        self.assertEqual("feishu:oc_test", result["session_key"])
        self.assertEqual("unbound", result["data"]["status"])

    def test_cc_control_accepts_approved_data_envelope(self) -> None:
        class FakeRunner:
            def json(self, args, **_kwargs):
                payload = {
                    "schema_version": 1,
                    "ok": True,
                    "project": "workspace",
                    "session_key": "feishu:oc_test",
                    "data": {"worktree": "/workspace/task"},
                }
                command = tuple(str(item) for item in args)
                return lifecycle.CompletedCommand(command, 0, "{}", ""), payload

        result = lifecycle.cc_control(
            FakeRunner(),
            "cc-connect",
            "sessions",
            "status",
            project="workspace",
            session_key="feishu:oc_test",
            data_dir=Path("/data"),
        )

        self.assertEqual("/workspace/task", result["data"]["worktree"])

    def test_cc_status_requires_matching_route_and_native_session(self) -> None:
        workspace = {
            "data": {
                "project_route": {"workspace": "/workspace/task"},
                "shared_route": None,
                "effective_route": {"workspace": "/workspace/task"},
            }
        }
        sessions = {
            "data": {
                "workspace": "/workspace/task",
                "agent_session_id": "thread-exact",
            }
        }

        lifecycle.verify_cc_state(
            workspace, sessions, Path("/workspace/task"), "thread-exact"
        )

        sessions["data"]["agent_session_id"] = "thread-other"
        with self.assertRaises(lifecycle.LifecycleError):
            lifecycle.verify_cc_state(
                workspace, sessions, Path("/workspace/task"), "thread-exact"
            )

    def test_cc_attachment_reverse_resolves_exact_active_native_thread(self) -> None:
        self.write_cc_attachment("thread-current")

        result = lifecycle.discover_active_cc_attachment(
            self.fixture.workspace / "cc-data",
            "workspace",
            "thread-current",
        )

        self.assertEqual("feishu:oc_stable", result["sessionKey"])
        self.assertEqual("s3", result["internalSessionId"])
        self.assertTrue(result["verified"])

    def test_cc_attachment_rejects_multiple_active_current_thread_routes(self) -> None:
        self.write_cc_attachment("thread-current")
        second = self.fixture.workspace / "cc-data" / "sessions" / "workspace_other.json"
        second.write_text(
            json.dumps(
                {
                    "sessions": {
                        "s8": {
                            "id": "s8",
                            "agent_session_id": "thread-current",
                        }
                    },
                    "active_session": {"feishu:oc_other": "s8"},
                    "user_sessions": {"feishu:oc_other": ["s8"]},
                }
            ),
            encoding="utf-8",
        )

        with self.assertRaises(lifecycle.LifecycleError) as caught:
            lifecycle.discover_active_cc_attachment(
                self.fixture.workspace / "cc-data",
                "workspace",
                "thread-current",
            )

        self.assertEqual("cc_control_conflict", caught.exception.code)

    def test_finish_selects_bound_invoking_legacy_thread(self) -> None:
        worktree = Path("/workspace/task")
        workspace = {
            "data": {
                "project_route": {"worktree": str(worktree)},
                "shared_route": None,
                "effective_route": {"worktree": str(worktree)},
            }
        }
        sessions = {
            "data": {
                "worktree": str(worktree),
                "agent_session_id": "thread-legacy",
                "busy": True,
            }
        }
        inspected = {
            "threadId": "thread-legacy",
            "name": "legacy task name",
            "cwd": str(worktree),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }

        with mock.patch.dict(
            lifecycle.os.environ, {"CODEX_THREAD_ID": "thread-legacy"}
        ):
            with mock.patch.object(
                lifecycle, "codex_command", return_value=inspected
            ) as codex:
                result = lifecycle.resolve_bound_finish_thread(
                    lifecycle.CommandRunner(),
                    Path("/control"),
                    worktree,
                    None,
                    workspace,
                    sessions,
                    None,
                )

        self.assertEqual("thread-legacy", result["threadId"])
        self.assertEqual("bound-invoking-thread", result["identitySource"])
        self.assertEqual("inspect", codex.call_args.args[2])

    def test_finish_selects_invoking_legacy_thread_without_task_init(self) -> None:
        worktree = Path("/workspace/task")
        inspected = {
            "threadId": "thread-current",
            "name": "legacy task name",
            "cwd": str(worktree),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }

        with mock.patch.dict(
            lifecycle.os.environ, {"CODEX_THREAD_ID": "thread-current"}
        ):
            with mock.patch.object(
                lifecycle, "codex_command", return_value=inspected
            ):
                result = lifecycle.resolve_invoking_finish_thread(
                    lifecycle.CommandRunner(),
                    Path("/control"),
                    worktree,
                    None,
                )

        self.assertEqual("thread-current", result["threadId"])
        self.assertEqual("invoking-thread", result["identitySource"])

    def test_finish_rejects_bound_legacy_thread_from_another_invoker(self) -> None:
        worktree = Path("/workspace/task")
        workspace = {
            "data": {
                "project_route": {"worktree": str(worktree)},
                "shared_route": None,
                "effective_route": {"worktree": str(worktree)},
            }
        }
        sessions = {
            "data": {
                "worktree": str(worktree),
                "agent_session_id": "thread-bound",
            }
        }

        with mock.patch.dict(
            lifecycle.os.environ, {"CODEX_THREAD_ID": "thread-other"}
        ):
            with self.assertRaises(lifecycle.LifecycleError) as caught:
                lifecycle.resolve_bound_finish_thread(
                    lifecycle.CommandRunner(),
                    Path("/control"),
                    worktree,
                    None,
                    workspace,
                    sessions,
                    None,
                )

        self.assertEqual("cc_control_conflict", caught.exception.code)

    def test_finish_rejects_exact_and_bound_thread_mismatch(self) -> None:
        worktree = Path("/workspace/task")
        workspace = {
            "data": {
                "project_route": {"worktree": str(worktree)},
                "shared_route": None,
                "effective_route": {"worktree": str(worktree)},
            }
        }
        sessions = {
            "data": {
                "worktree": str(worktree),
                "agent_session_id": "thread-bound",
            }
        }

        with self.assertRaises(lifecycle.LifecycleError) as caught:
            lifecycle.resolve_bound_finish_thread(
                lifecycle.CommandRunner(),
                Path("/control"),
                worktree,
                {"threadId": "thread-exact"},
                workspace,
                sessions,
                None,
            )

        self.assertEqual("cc_control_conflict", caught.exception.code)

    def test_finish_still_blocks_unrelated_active_thread(self) -> None:
        worktree = Path("/workspace/task")
        listed = {
            "threads": [
                {
                    "threadId": "thread-target",
                    "cwd": str(worktree),
                    "ephemeral": False,
                    "status": "active",
                },
                {
                    "threadId": "thread-other",
                    "cwd": str(worktree),
                    "ephemeral": False,
                    "status": "active",
                },
            ]
        }

        self.assertEqual(
            ["thread-other"],
            lifecycle.active_other_thread_ids(
                listed, worktree, "thread-target"
            ),
        )

    def test_finish_cleanup_allows_bound_invoking_legacy_thread(self) -> None:
        worktree = self.fixture.worktree()
        args = type("Args", (), {
            "workspace_root": str(self.fixture.workspace),
            "repo": str(self.fixture.repo),
            "branch": "ws/feat/example",
            "group_name": None,
            "project": "workspace",
            "record_url": None,
            "document_ready": False,
            "codex_control": str(
                Path(__file__).resolve().parents[1]
                / "scripts"
                / "codex-task-control"
            ),
            "codex_socket": None,
            "cc_connect": "cc-connect",
            "cc_data_dir": str(self.fixture.workspace),
            "lark_cli": "lark-cli",
        })()
        route = {
            "data": {
                "project_route": {"worktree": str(worktree)},
                "shared_route": None,
                "effective_route": {"worktree": str(worktree)},
            }
        }
        unbound_route = {
            "data": {
                "project_route": None,
                "shared_route": None,
                "effective_route": None,
            }
        }
        sessions = {
            "data": {
                "worktree": str(worktree),
                "agent_session_id": "thread-legacy",
                "busy": True,
            }
        }
        events = []
        workspace_statuses = iter((route, unbound_route))

        def cc(_runner, _executable, domain, operation, **_kwargs):
            events.append(f"{domain}.{operation}")
            if (domain, operation) == ("workspace", "status"):
                return next(workspace_statuses)
            if (domain, operation) == ("sessions", "status"):
                return sessions
            if (domain, operation) == ("workspace", "unbind"):
                return {
                    "data": {
                        "closeout_guard": {
                            "busy": True,
                            "verified": True,
                            "expected_agent_session_id": "thread-legacy",
                            "active_agent_session_id": "thread-legacy",
                            "live_agent_session_id": "thread-legacy",
                            "active_turn_preserved": True,
                        }
                    }
                }
            raise AssertionError(f"unexpected cc operation: {domain}.{operation}")

        def codex(_runner, _executable, command, **_kwargs):
            events.append(f"codex.{command}")
            thread = {
                "threadId": "thread-legacy",
                "name": "legacy task name",
                "cwd": str(worktree),
                "ephemeral": False,
                "status": "active",
                "verified": True,
            }
            if command == "inspect":
                return thread
            if command == "list-threads":
                return {"threads": [thread], "verified": True}
            raise AssertionError(f"unexpected Codex command: {command}")

        with mock.patch.dict(
            lifecycle.os.environ, {"CODEX_THREAD_ID": "thread-legacy"}
        ):
            with mock.patch.object(lifecycle, "discover_thread", return_value=None):
                with mock.patch.object(
                    lifecycle,
                    "discover_group",
                    return_value={"chatId": "oc_test"},
                ):
                    with mock.patch.object(lifecycle, "cc_control", side_effect=cc):
                        with mock.patch.object(
                            lifecycle, "codex_command", side_effect=codex
                        ):
                            result = lifecycle.finish_cleanup_task(
                                args, lifecycle.CommandRunner()
                            )

        self.assertEqual("thread-legacy", result["closeThreadId"])
        self.assertTrue(result["cleanup"]["worktreeRemoved"])
        self.assertFalse(worktree.exists())
        self.assertIn("workspace.unbind", events)

    def test_stable_checkout_cleanup_preserves_dirty_files_and_directories(self) -> None:
        args = self.stable_finish_args()
        (self.fixture.repo / "README.md").write_text("dirty but preserved\n")
        local_file = self.fixture.repo / "local" / "artifact.bin"
        local_file.parent.mkdir()
        local_file.write_bytes(b"keep")
        archive_file = self.fixture.repo.parent / "archive" / "keep.bin"
        archive_file.parent.mkdir()
        archive_file.write_bytes(b"archive")
        thread = {
            "threadId": "thread-current",
            "name": "operational task",
            "cwd": str(self.fixture.repo),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }

        with mock.patch.dict(
            lifecycle.os.environ, {"CODEX_THREAD_ID": "thread-current"}
        ):
            with mock.patch.object(
                lifecycle, "codex_command", return_value=thread
            ):
                with mock.patch.object(
                    lifecycle,
                    "purge_expired_archives",
                    side_effect=AssertionError("stable cleanup must not purge archives"),
                ):
                    with mock.patch.object(
                        lifecycle,
                        "archive_and_remove_worktree",
                        side_effect=AssertionError(
                            "stable cleanup must not archive or remove a directory"
                        ),
                    ):
                        result = lifecycle.finish_cleanup_task(
                            args, lifecycle.CommandRunner()
                        )

        self.assertEqual("stable-checkout", result["targetMode"])
        self.assertEqual("thread-current", result["closeThreadId"])
        self.assertFalse(result["cleanup"]["worktreeRemoved"])
        self.assertTrue(result["cleanup"]["stableCheckout"]["preserved"])
        self.assertTrue(result["cleanup"]["stableCheckoutPreserved"])
        self.assertIsNone(result["cleanup"]["localArchive"])
        self.assertFalse(result["cleanup"]["directoryArchive"]["applicable"])
        self.assertFalse(result["cleanup"]["mainIntegration"]["applicable"])
        self.assertEqual("dirty but preserved\n", (self.fixture.repo / "README.md").read_text())
        self.assertEqual(b"keep", local_file.read_bytes())
        self.assertEqual(b"archive", archive_file.read_bytes())

    def test_stable_checkout_cleanup_rejects_current_thread_from_another_cwd(self) -> None:
        args = self.stable_finish_args()
        thread = {
            "threadId": "thread-current",
            "name": "other task",
            "cwd": str(self.fixture.repo.parent),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }

        with mock.patch.dict(
            lifecycle.os.environ, {"CODEX_THREAD_ID": "thread-current"}
        ):
            with mock.patch.object(
                lifecycle, "codex_command", return_value=thread
            ):
                with self.assertRaises(lifecycle.LifecycleError) as caught:
                    lifecycle.finish_cleanup_task(args, lifecycle.CommandRunner())

        self.assertEqual("thread_conflict", caught.exception.code)
        self.assertTrue(self.fixture.repo.is_dir())

    def test_stable_checkout_cleanup_unbinds_only_exact_current_attachment(self) -> None:
        args = self.stable_finish_args()
        self.write_cc_attachment("thread-current")
        thread = {
            "threadId": "thread-current",
            "name": "operational task",
            "cwd": str(self.fixture.repo),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }
        route = {
            "data": {
                "project_route": {"worktree": str(self.fixture.repo)},
                "shared_route": None,
                "effective_route": {"worktree": str(self.fixture.repo)},
            }
        }
        sessions = {
            "data": {
                "worktree": str(self.fixture.repo),
                "agent_session_id": "thread-current",
                "busy": True,
            }
        }
        unbound_route = {
            "data": {
                "project_route": None,
                "shared_route": None,
                "effective_route": None,
            }
        }
        statuses = iter((route, unbound_route))
        unbind_kwargs = {}

        def cc(_runner, _executable, domain, operation, **kwargs):
            if (domain, operation) == ("workspace", "status"):
                return next(statuses)
            if (domain, operation) == ("sessions", "status"):
                return sessions
            if (domain, operation) == ("workspace", "unbind"):
                unbind_kwargs.update(kwargs)
                return {
                    "data": {
                        "closeout_guard": {
                            "busy": True,
                            "verified": True,
                            "expected_agent_session_id": "thread-current",
                            "active_agent_session_id": "thread-current",
                            "live_agent_session_id": "thread-current",
                            "active_turn_preserved": True,
                        }
                    }
                }
            raise AssertionError(f"unexpected cc operation: {domain}.{operation}")

        with mock.patch.dict(
            lifecycle.os.environ, {"CODEX_THREAD_ID": "thread-current"}
        ):
            with mock.patch.object(
                lifecycle, "codex_command", return_value=thread
            ):
                with mock.patch.object(
                    lifecycle,
                    "verify_group_members",
                    return_value={"verified": True},
                ):
                    with mock.patch.object(lifecycle, "cc_control", side_effect=cc):
                        result = lifecycle.finish_cleanup_task(
                            args, lifecycle.CommandRunner()
                        )

        self.assertEqual("feishu:oc_stable", result["closeSessionKey"])
        self.assertEqual(self.fixture.repo, unbind_kwargs["worktree"])
        self.assertEqual(
            "thread-current", unbind_kwargs["expected_agent_session_id"]
        )
        self.assertTrue(self.fixture.repo.is_dir())

    def test_finish_cleanup_archives_all_untracked_content_and_detaches_worktree(self) -> None:
        (self.fixture.repo / ".gitignore").write_text(".cache/\n*.pyc\n")
        self.fixture.git("add", ".gitignore")
        self.fixture.git("commit", "-qm", "ignore generated content")
        worktree = self.fixture.worktree()
        (worktree / ".cache" / "nested").mkdir(parents=True)
        (worktree / ".cache" / "nested" / "artifact.bin").write_bytes(b"artifact")
        (worktree / "generated.pyc").write_bytes(b"cache")
        (worktree / "handoff.md").write_text("legacy handoff without a marker\n")
        (worktree / "draft.go").write_text("package draft\n")
        args = type("Args", (), {
            "workspace_root": str(self.fixture.workspace),
            "repo": str(self.fixture.repo),
            "branch": "ws/feat/example",
            "group_name": None,
            "project": "workspace",
            "record_url": None,
            "document_ready": False,
            "codex_control": str(
                Path(__file__).resolve().parents[1]
                / "scripts"
                / "codex-task-control"
            ),
            "codex_socket": None,
            "cc_connect": "cc-connect",
            "cc_data_dir": str(self.fixture.workspace),
            "lark_cli": "lark-cli",
        })()
        thread = {
            "threadId": "01a00000-0000-7000-8000-000000000001",
            "name": "ws/feat/example",
            "cwd": str(worktree),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }

        with mock.patch.object(lifecycle, "discover_thread", return_value=thread):
            with mock.patch.object(lifecycle, "discover_group", return_value=None):
                with mock.patch.object(
                    lifecycle,
                    "codex_command",
                    return_value={"threads": [thread], "verified": True},
                ):
                    result = lifecycle.finish_cleanup_task(
                        args, lifecycle.CommandRunner()
                    )

        archive = result["cleanup"]["localArchive"]
        archive_path = Path(archive["path"])
        self.assertEqual(self.fixture.repo.parent / "archive", archive_path.parent.parent)
        self.assertTrue((archive_path / "files" / ".cache" / "nested" / "artifact.bin").is_file())
        self.assertEqual(b"cache", (archive_path / "files" / "generated.pyc").read_bytes())
        self.assertEqual(
            "legacy handoff without a marker\n",
            (archive_path / "files" / "handoff.md").read_text(),
        )
        self.assertEqual(
            "package draft\n", (archive_path / "files" / "draft.go").read_text()
        )
        self.assertEqual(archive, result["cleanup"]["ignoredArchive"])
        self.assertFalse((archive_path / ".git").exists())
        self.assertTrue(archive["worktreeDetached"])
        manifest = json.loads(
            (archive_path / "manifest.json").read_text(encoding="utf-8")
        )
        self.assertEqual("complete", manifest["state"])
        self.assertTrue(manifest["worktreeDetached"])
        self.assertFalse(worktree.exists())
        self.assertFalse(
            any(
                item.get("resolved") == str(worktree)
                for item in lifecycle.worktree_inventory(
                    lifecycle.CommandRunner(), self.fixture.repo
                )
            )
        )

    def test_local_archive_rolls_back_when_worktree_remove_fails(self) -> None:
        (self.fixture.repo / ".gitignore").write_text(".cache/\n")
        self.fixture.git("add", ".gitignore")
        self.fixture.git("commit", "-qm", "ignore generated content")
        worktree = self.fixture.worktree()
        artifact = worktree / ".cache" / "artifact.bin"
        artifact.parent.mkdir()
        artifact.write_bytes(b"artifact")
        draft = worktree / "draft.txt"
        draft.write_text("draft\n")

        with mock.patch.object(
            lifecycle,
            "remove_linked_worktree",
            side_effect=lifecycle.LifecycleError(
                "worktree_remove_failed", "simulated failure"
            ),
        ):
            with self.assertRaises(lifecycle.LifecycleError) as caught:
                lifecycle.archive_and_remove_worktree(
                    lifecycle.CommandRunner(),
                    self.fixture.repo,
                    self.fixture.workspace,
                    worktree,
                    "ws/feat/example",
                    self.fixture.git("rev-parse", "HEAD").stdout.strip(),
                    "thread-rollback",
                    now=datetime(2026, 8, 15, tzinfo=timezone.utc),
                )

        self.assertEqual("worktree_remove_failed", caught.exception.code)
        self.assertEqual(b"artifact", artifact.read_bytes())
        self.assertEqual("draft\n", draft.read_text())
        month = self.fixture.repo.parent / "archive" / "2026-08"
        self.assertFalse(any(path.name.endswith(".partial") for path in month.iterdir()))
        self.assertTrue(worktree.exists())

    def test_expired_archive_purge_removes_only_complete_detached_entries(self) -> None:
        (self.fixture.repo / ".gitignore").write_text(".cache/\n")
        self.fixture.git("add", ".gitignore")
        self.fixture.git("commit", "-qm", "ignore generated content")
        worktree = self.fixture.worktree()
        artifact = worktree / ".cache" / "artifact.bin"
        artifact.parent.mkdir()
        artifact.write_bytes(b"artifact")
        archive, _ = lifecycle.archive_and_remove_worktree(
            lifecycle.CommandRunner(),
            self.fixture.repo,
            self.fixture.workspace,
            worktree,
            "ws/feat/example",
            self.fixture.git("rev-parse", "HEAD").stdout.strip(),
            "thread-expired",
            now=datetime(2026, 5, 15, tzinfo=timezone.utc),
        )
        archive_path = Path(archive["path"])
        incomplete = archive_path.parent / ".interrupted.partial"
        incomplete.mkdir(mode=0o700)
        (incomplete / "important.bin").write_bytes(b"keep")

        result = lifecycle.purge_expired_archives(
            self.fixture.repo,
            self.fixture.workspace,
            now=datetime(2026, 8, 1, tzinfo=timezone.utc),
        )

        self.assertIn(str(archive_path), result["removed"])
        self.assertFalse(archive_path.exists())
        self.assertTrue((incomplete / "important.bin").exists())
        self.assertIn(str(incomplete), [item["path"] for item in result["skipped"]])

    def test_finish_cleanup_blocks_unrelated_active_thread_before_unbind(self) -> None:
        worktree = self.fixture.worktree()
        args = type("Args", (), {
            "workspace_root": str(self.fixture.workspace),
            "repo": str(self.fixture.repo),
            "branch": "ws/feat/example",
            "group_name": None,
            "project": "workspace",
            "record_url": None,
            "document_ready": False,
            "codex_control": str(
                Path(__file__).resolve().parents[1]
                / "scripts"
                / "codex-task-control"
            ),
            "codex_socket": None,
            "cc_connect": "cc-connect",
            "cc_data_dir": str(self.fixture.workspace),
            "lark_cli": "lark-cli",
        })()
        route = {
            "data": {
                "project_route": {"worktree": str(worktree)},
                "shared_route": None,
                "effective_route": {"worktree": str(worktree)},
            }
        }
        sessions = {
            "data": {
                "worktree": str(worktree),
                "agent_session_id": "thread-exact",
                "busy": True,
            }
        }
        cc_operations = []

        def cc(_runner, _executable, domain, operation, **_kwargs):
            cc_operations.append(f"{domain}.{operation}")
            if (domain, operation) == ("workspace", "status"):
                return route
            if (domain, operation) == ("sessions", "status"):
                return sessions
            raise AssertionError("cleanup mutated cc-connect before conflict detection")

        exact = {
            "threadId": "thread-exact",
            "name": "ws/feat/example",
            "cwd": str(worktree),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }
        other = {
            "threadId": "thread-other",
            "name": "unrelated",
            "cwd": str(worktree),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }

        with mock.patch.object(lifecycle, "discover_thread", return_value=exact):
            with mock.patch.object(
                lifecycle,
                "discover_group",
                return_value={"chatId": "oc_test"},
            ):
                with mock.patch.object(lifecycle, "cc_control", side_effect=cc):
                    with mock.patch.object(
                        lifecycle,
                        "codex_command",
                        return_value={
                            "threads": [exact, other],
                            "verified": True,
                        },
                    ):
                        with self.assertRaises(
                            lifecycle.LifecycleError
                        ) as caught:
                            lifecycle.finish_cleanup_task(
                                args, lifecycle.CommandRunner()
                            )

        self.assertEqual("active_thread_conflict", caught.exception.code)
        self.assertEqual(["thread-other"], caught.exception.details["threadIds"])
        self.assertNotIn("workspace.unbind", cc_operations)
        self.assertTrue(worktree.exists())

    def test_task_record_url_has_no_label_suffix_or_query(self) -> None:
        url = "https://example.larkoffice.com/docx/abc123"
        self.assertEqual(
            f"Task record: {url}", lifecycle.canonical_record_message(url)
        )
        for invalid in (
            "https://example.larkoffice.com/docx/abc123|title",
            "https://example.larkoffice.com/docx/abc123?title=x",
            "<https://example.larkoffice.com/docx/abc123|title>",
        ):
            with self.assertRaises(lifecycle.LifecycleError):
                lifecycle.canonical_record_message(invalid)

    def test_remote_url_host_supports_git_url_forms_and_exact_hosts(self) -> None:
        expected = {
            "git@git.example.com:data-arch/demo.git": "git.example.com",
            "ssh://git@git.example.com/data-arch/demo.git": "git.example.com",
            "https://GIT.EXAMPLE.COM./data-arch/demo.git": "git.example.com",
            "https://evilgit.example.com/data-arch/demo.git": "evilgit.example.com",
            "git@git.example.com.invalid:data-arch/demo.git": "git.example.com.invalid",
            "/tmp/demo.git": None,
        }
        for remote_url, host in expected.items():
            with self.subTest(remote_url=remote_url):
                self.assertEqual(host, lifecycle.remote_url_host(remote_url))

    def test_task_document_policy_requires_configured_exact_remote(self) -> None:
        runner = lifecycle.CommandRunner()
        stable, _ = lifecycle.validate_stable_repo(
            runner, str(self.fixture.repo), self.fixture.workspace
        )
        with mock.patch.object(
            lifecycle, "DOCUMENT_REMOTE_HOSTS", frozenset({"git.example.com"})
        ):
            policy = lifecycle.task_document_policy(runner, stable)
            self.assertFalse(policy["enabled"])
            self.assertEqual([], policy["remoteHosts"])
            self.assertEqual([], policy["matchedHosts"])

            self.fixture.git(
                "remote", "add", "origin", "https://github.com/example/demo.git"
            )
            policy = lifecycle.task_document_policy(runner, stable)
            self.assertFalse(policy["enabled"])
            self.assertEqual(["github.com"], policy["remoteHosts"])

            self.fixture.git(
                "remote", "set-url", "origin", "git@git.example.com:data-arch/demo.git"
            )
            policy = lifecycle.task_document_policy(runner, stable)
            self.assertTrue(policy["enabled"])
            self.assertEqual("required", policy["status"])
            self.assertEqual(["git.example.com"], policy["remoteHosts"])
            self.assertEqual(["git.example.com"], policy["matchedHosts"])

    def test_runtime_configuration_reports_missing_personal_and_document_values(self) -> None:
        with mock.patch.multiple(
            lifecycle,
            LARK_CLI_BOT_APP_ID="",
            CURRENT_USER_OPEN_ID="",
            CC_CONNECT_BOT_APP_ID="",
            DEFAULT_CC_PROJECT="",
            DOCUMENT_REMOTE_HOSTS=frozenset({"git.example.com"}),
        ):
            with mock.patch.dict(lifecycle.os.environ, {}, clear=True):
                with self.assertRaises(lifecycle.LifecycleError) as caught:
                    lifecycle.validate_runtime_configuration()

        self.assertEqual("configuration_missing", caught.exception.code)
        self.assertEqual(
            [
                "CC_CONNECT_TASK_BOT_APP_ID",
                "CC_CONNECT_TASK_DOCUMENT_FOLDER_TOKEN",
                "CC_CONNECT_TASK_DOCUMENT_FOLDER_URL",
                "CC_CONNECT_TASK_LARK_CLI_BOT_APP_ID",
                "CC_CONNECT_TASK_PROJECT",
                "CC_CONNECT_TASK_USER_OPEN_ID",
            ],
            caught.exception.details["missingEnvironmentVariables"],
        )

    def test_runtime_configuration_accepts_generic_config(self) -> None:
        with mock.patch.multiple(
            lifecycle,
            LARK_CLI_BOT_APP_ID="cli_lark",
            CURRENT_USER_OPEN_ID="ou_user",
            CC_CONNECT_BOT_APP_ID="cli_cc",
            DEFAULT_CC_PROJECT="automation",
            DOCUMENT_REMOTE_HOSTS=frozenset({"git.example.com"}),
        ):
            with mock.patch.dict(
                lifecycle.os.environ,
                {
                    "CC_CONNECT_TASK_DOCUMENT_FOLDER_URL": "https://docs.example/folder",
                    "CC_CONNECT_TASK_DOCUMENT_FOLDER_TOKEN": "folder_token",
                },
                clear=True,
            ):
                lifecycle.validate_runtime_configuration()

    def test_finish_document_gates_follow_repository_policy(self) -> None:
        url = "https://example.larkoffice.com/docx/abc123"
        standard = {"enabled": True}
        non_codebase = {"enabled": False}

        self.assertEqual(
            url,
            lifecycle.validate_finish_document_input(standard, url, True),
        )
        lifecycle.validate_finish_document_finalized(standard, True)
        self.assertIsNone(
            lifecycle.validate_finish_document_input(non_codebase, None, False)
        )
        lifecycle.validate_finish_document_finalized(non_codebase, False)

        with self.assertRaises(lifecycle.LifecycleError):
            lifecycle.validate_finish_document_input(standard, None, False)
        with self.assertRaises(lifecycle.LifecycleError):
            lifecycle.validate_finish_document_input(non_codebase, url, True)
        with self.assertRaises(lifecycle.LifecycleError):
            lifecycle.validate_finish_document_finalized(non_codebase, True)

    def test_finish_parser_allows_policy_disabled_document_flags(self) -> None:
        args = lifecycle.build_parser().parse_args(
            [
                "finish-cleanup",
                "--repo",
                "/workspace/demo/demo",
                "--branch",
                "ws/feat/example",
            ]
        )

        self.assertIsNone(args.record_url)
        self.assertFalse(args.document_ready)

    def test_finish_close_parser_accepts_cleanup_thread_id(self) -> None:
        args = lifecycle.build_parser().parse_args(
            [
                "finish-close",
                "--repo",
                "/workspace/demo/demo",
                "--branch",
                "ws/feat/example",
                "--thread-id",
                "thread-exact",
            ]
        )

        self.assertEqual("thread-exact", args.thread_id)

    def test_finish_parsers_accept_explicit_stable_checkout_without_branch(self) -> None:
        for command in ("finish-handoff", "finish-cleanup", "finish-close"):
            with self.subTest(command=command):
                args = lifecycle.build_parser().parse_args(
                    [
                        command,
                        "--repo",
                        "/workspace/demo/demo",
                        "--stable-checkout",
                    ]
                )
                self.assertTrue(args.stable_checkout)
                self.assertIsNone(args.branch)

    def test_finish_parser_rejects_branch_and_stable_checkout_together(self) -> None:
        with self.assertRaises(SystemExit):
            lifecycle.build_parser().parse_args(
                [
                    "finish-cleanup",
                    "--repo",
                    "/workspace/demo/demo",
                    "--branch",
                    "ws/feat/example",
                    "--stable-checkout",
                ]
            )

    def test_stable_checkout_close_removes_shortcut_before_current_thread_archive(self) -> None:
        args = self.stable_finish_args(
            thread_id="thread-current", session_key="feishu:oc_stable"
        )
        self.write_cc_attachment("thread-current")
        thread = {
            "threadId": "thread-current",
            "name": "operational task",
            "cwd": str(self.fixture.repo),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }
        events = []

        def codex(_runner, _executable, command, **_kwargs):
            if command == "inspect":
                return thread
            if command == "archive":
                events.append("archive")
                return {
                    "archived": False,
                    "scheduled": True,
                    "verified": True,
                }
            raise AssertionError(f"unexpected Codex command: {command}")

        def remove_shortcut(_runner, _lark_cli, chat_id):
            events.append(f"shortcut:{chat_id}")
            return {"present": False, "changed": True, "verified": True}

        with mock.patch.dict(
            lifecycle.os.environ, {"CODEX_THREAD_ID": "thread-current"}
        ):
            with mock.patch.object(lifecycle, "codex_command", side_effect=codex):
                with mock.patch.object(
                    lifecycle,
                    "verify_group_members",
                    return_value={"verified": True},
                ):
                    with mock.patch.object(
                        lifecycle,
                        "remove_feed_shortcut",
                        side_effect=remove_shortcut,
                    ):
                        result = lifecycle.finish_close_task(
                            args, lifecycle.CommandRunner()
                        )

        self.assertEqual(["shortcut:oc_stable", "archive"], events)
        self.assertEqual("stable-checkout", result["targetMode"])
        self.assertTrue(result["stableCheckout"]["preserved"])
        self.assertTrue(self.fixture.repo.is_dir())

    def test_stable_checkout_close_rejects_another_thread_id(self) -> None:
        args = self.stable_finish_args(thread_id="thread-other")
        thread = {
            "threadId": "thread-current",
            "name": "operational task",
            "cwd": str(self.fixture.repo),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }

        with mock.patch.dict(
            lifecycle.os.environ, {"CODEX_THREAD_ID": "thread-current"}
        ):
            with mock.patch.object(
                lifecycle, "codex_command", return_value=thread
            ):
                with self.assertRaises(lifecycle.LifecycleError) as caught:
                    lifecycle.finish_close_task(args, lifecycle.CommandRunner())

        self.assertEqual("thread_conflict", caught.exception.code)
        self.assertTrue(self.fixture.repo.is_dir())

    def test_worktree_creation_is_idempotent(self) -> None:
        runner = lifecycle.CommandRunner()
        stable, common = lifecycle.validate_stable_repo(
            runner, str(self.fixture.repo), self.fixture.workspace
        )

        first = lifecycle.ensure_worktree(
            runner,
            stable,
            common,
            self.fixture.workspace,
            "ws/feat/example",
            "main",
        )
        second = lifecycle.ensure_worktree(
            runner,
            stable,
            common,
            self.fixture.workspace,
            "ws/feat/example",
            "main",
        )

        self.assertTrue(first["created"])
        self.assertFalse(second["created"])
        self.assertEqual(first["path"], second["path"])

    def test_main_integration_accepts_task_head_ancestor(self) -> None:
        worktree = self.fixture.worktree()
        task_head = self.fixture.commit_file(
            worktree, "task.txt", "task\n", "task change"
        )
        self.fixture.git("merge", "--ff-only", task_head)
        main_head = self.fixture.git("rev-parse", "HEAD").stdout.strip()

        result = lifecycle.task_changes_in_main(
            lifecycle.CommandRunner(), self.fixture.repo, task_head, main_head
        )

        self.assertTrue(result["contained"])
        self.assertEqual("ancestor", result["method"])

    def test_main_integration_accepts_squashed_final_content(self) -> None:
        worktree = self.fixture.worktree()
        self.fixture.commit_file(
            worktree, "first.txt", "first\n", "first task change"
        )
        task_head = self.fixture.commit_file(
            worktree, "second.txt", "second\n", "second task change"
        )
        self.fixture.git("merge", "--squash", task_head)
        self.fixture.git("commit", "-qm", "squash task changes")
        main_head = self.fixture.git("rev-parse", "HEAD").stdout.strip()
        ancestry = self.fixture.git(
            "merge-base", "--is-ancestor", task_head, main_head, check=False
        )

        result = lifecycle.task_changes_in_main(
            lifecycle.CommandRunner(), self.fixture.repo, task_head, main_head
        )

        self.assertEqual(1, ancestry.returncode)
        self.assertTrue(result["contained"])
        self.assertEqual("changed-path-tree", result["method"])
        self.assertEqual([], result["differingPaths"])

    def test_main_integration_rejects_unintegrated_path(self) -> None:
        worktree = self.fixture.worktree()
        task_head = self.fixture.commit_file(
            worktree, "task.txt", "task\n", "task change"
        )
        main_head = self.fixture.git("rev-parse", "HEAD").stdout.strip()

        result = lifecycle.task_changes_in_main(
            lifecycle.CommandRunner(), self.fixture.repo, task_head, main_head
        )

        self.assertFalse(result["contained"])
        self.assertEqual(["task.txt"], result["differingPaths"])

    def test_main_integration_refreshes_stable_before_accepting(self) -> None:
        self.fixture.add_local_origin()
        worktree = self.fixture.worktree()
        task_head = self.fixture.commit_file(
            worktree, "task.txt", "task\n", "task change"
        )
        self.fixture.git_at(worktree, "push", "-q", "origin", "HEAD:main")

        result = lifecycle.ensure_task_changes_in_main(
            lifecycle.CommandRunner(),
            self.fixture.repo,
            "ws/feat/example",
            task_head,
        )

        self.assertTrue(result["final"]["contained"])
        self.assertTrue(result["refresh"]["updated"])
        self.assertEqual(task_head, self.fixture.git("rev-parse", "main").stdout.strip())

    def test_finish_blocks_after_latest_main_still_lacks_task(self) -> None:
        self.fixture.add_local_origin()
        worktree = self.fixture.worktree()
        task_head = self.fixture.commit_file(
            worktree, "task.txt", "task\n", "task change"
        )
        remote_worktree = Path(self.fixture.temp.name) / "remote-main"
        self.fixture.git(
            "worktree",
            "add",
            "-q",
            "-b",
            "remote-main-update",
            str(remote_worktree),
            "main",
        )
        remote_head = self.fixture.commit_file(
            remote_worktree, "remote.txt", "remote\n", "remote main change"
        )
        self.fixture.git_at(
            remote_worktree, "push", "-q", "origin", "HEAD:main"
        )
        self.fixture.git("worktree", "remove", str(remote_worktree))
        handoff = worktree / "handoff.md"
        handoff.write_text(
            "# Task Handoff\n\n"
            "The target task and thread are already initialized. Lifecycle wording "
            "was removed from the effective request.\n"
        )
        args = type("Args", (), {
            "workspace_root": str(self.fixture.workspace),
            "repo": str(self.fixture.repo),
            "branch": "ws/feat/example",
            "group_name": None,
            "project": "workspace",
            "record_url": None,
            "document_ready": False,
            "codex_control": str(
                Path(__file__).resolve().parents[1]
                / "scripts"
                / "codex-task-control"
            ),
            "codex_socket": None,
            "cc_connect": "cc-connect",
            "cc_data_dir": str(self.fixture.workspace),
            "lark_cli": "lark-cli",
        })()
        thread = {
            "threadId": "thread-exact",
            "name": "ws/feat/example",
            "cwd": str(worktree),
            "ephemeral": False,
            "status": "active",
            "verified": True,
        }
        cc = mock.Mock(side_effect=AssertionError("cc-connect mutated before main gate"))

        with mock.patch.object(lifecycle, "discover_thread", return_value=thread):
            with mock.patch.object(
                lifecycle, "discover_group", return_value={"chatId": "oc_test"}
            ):
                with mock.patch.object(lifecycle, "cc_control", cc):
                    with self.assertRaises(lifecycle.LifecycleError) as caught:
                        lifecycle.finish_cleanup_task(
                            args, lifecycle.CommandRunner()
                        )

        self.assertEqual("task_not_integrated", caught.exception.code)
        self.assertEqual(task_head, caught.exception.details["taskHead"])
        self.assertEqual(remote_head, self.fixture.git("rev-parse", "main").stdout.strip())
        self.assertTrue(worktree.exists())
        self.assertTrue(handoff.exists())
        cc.assert_not_called()

    def test_status_uses_live_sources_without_lifecycle_state(self) -> None:
        args = type("Args", (), {"branch": "ws/feat/example"})()
        evidence = {"worktree": {"path": "/task"}, "verified": True}
        with mock.patch.object(lifecycle, "live_task_evidence", return_value=evidence):
            result = lifecycle.task_status(args, lifecycle.CommandRunner())

        self.assertTrue(result["verified"])
        self.assertNotIn("stateFile", result)
        self.assertEqual("/task", result["worktree"]["path"])

    def test_finish_handoff_switches_default_before_exact_finish_queue(self) -> None:
        args = type("Args", (), {
            "branch": "ws/feat/example",
            "codex_control": None,
            "codex_socket": None,
        })()
        evidence = {
            "thread": {"threadId": "thread-exact", "status": "active"},
            "verified": True,
        }
        calls = []

        def codex(_runner, _executable, command, **kwargs):
            calls.append((command, tuple(kwargs.get("extra", ()))))
            if command == "default":
                return {"verified": True, "mode": "default"}
            if command == "queue":
                return {
                    "verified": True,
                    "queued": True,
                    "state": "pending",
                    "queuedSubmissionId": "submission-1",
                }
            raise AssertionError(f"unexpected Codex command: {command}")

        with mock.patch.object(lifecycle, "live_task_evidence", return_value=evidence):
            with mock.patch.object(
                lifecycle, "resolve_control_executable", return_value=Path("/control")
            ):
                with mock.patch.object(lifecycle, "codex_command", side_effect=codex):
                    result = lifecycle.finish_handoff_task(
                        args, lifecycle.CommandRunner()
                    )

        self.assertTrue(result["verified"])
        self.assertEqual("finish-handoff", result["operation"])
        self.assertEqual(
            [
                ("default", ()),
                ("queue", ("--pending-only", "$task finish")),
            ],
            calls,
        )

    def test_stable_checkout_finish_handoff_uses_current_thread_evidence(self) -> None:
        args = self.stable_finish_args()
        evidence = {
            "targetMode": "stable-checkout",
            "thread": {"threadId": "thread-current", "status": "active"},
            "verified": True,
        }
        calls = []

        def codex(_runner, _executable, command, **kwargs):
            calls.append((command, kwargs.get("thread_id")))
            if command == "default":
                return {"verified": True, "mode": "default"}
            if command == "queue":
                return {
                    "verified": True,
                    "queued": True,
                    "state": "pending",
                    "queuedSubmissionId": "submission-stable",
                }
            raise AssertionError(f"unexpected Codex command: {command}")

        with mock.patch.object(
            lifecycle, "live_stable_checkout_evidence", return_value=evidence
        ):
            with mock.patch.object(
                lifecycle,
                "live_task_evidence",
                side_effect=AssertionError("stable handoff must not resolve a task worktree"),
            ):
                with mock.patch.object(
                    lifecycle, "resolve_control_executable", return_value=Path("/control")
                ):
                    with mock.patch.object(
                        lifecycle, "codex_command", side_effect=codex
                    ):
                        result = lifecycle.finish_handoff_task(
                            args, lifecycle.CommandRunner()
                        )

        self.assertEqual(
            [("default", "thread-current"), ("queue", "thread-current")], calls
        )
        self.assertEqual("stable-checkout", result["targetMode"])
        self.assertIsNone(result["branch"])

    def test_finish_handoff_does_not_queue_without_confirmed_default_mode(self) -> None:
        args = type("Args", (), {
            "branch": "ws/feat/example",
            "codex_control": None,
            "codex_socket": None,
        })()
        evidence = {
            "thread": {"threadId": "thread-exact", "status": "active"},
            "verified": True,
        }
        codex = mock.Mock(return_value={"verified": True, "mode": "plan"})

        with mock.patch.object(lifecycle, "live_task_evidence", return_value=evidence):
            with mock.patch.object(
                lifecycle, "resolve_control_executable", return_value=Path("/control")
            ):
                with mock.patch.object(lifecycle, "codex_command", codex):
                    with self.assertRaises(lifecycle.LifecycleError) as caught:
                        lifecycle.finish_handoff_task(args, lifecycle.CommandRunner())

        self.assertEqual("mode_handoff_failed", caught.exception.code)
        self.assertEqual(1, codex.call_count)

    def test_finish_handoff_rejects_completed_turn_history_as_queue(self) -> None:
        args = type("Args", (), {
            "branch": "ws/feat/example",
            "codex_control": None,
            "codex_socket": None,
        })()
        evidence = {
            "thread": {"threadId": "thread-exact", "status": "active"},
            "verified": True,
        }
        codex = mock.Mock(side_effect=(
            {"verified": True, "mode": "default"},
            {
                "verified": True,
                "queued": True,
                "source": "turnHistory",
                "state": "completed",
                "turnId": "turn-old",
            },
        ))

        with mock.patch.object(lifecycle, "live_task_evidence", return_value=evidence):
            with mock.patch.object(
                lifecycle, "resolve_control_executable", return_value=Path("/control")
            ):
                with mock.patch.object(lifecycle, "codex_command", codex):
                    with self.assertRaises(lifecycle.LifecycleError) as caught:
                        lifecycle.finish_handoff_task(args, lifecycle.CommandRunner())

        self.assertEqual("continuation_queue_failed", caught.exception.code)
        self.assertEqual(2, codex.call_count)

    def test_parser_exposes_finish_handoff_without_custom_continuation(self) -> None:
        args = lifecycle.build_parser().parse_args(
            [
                "finish-handoff",
                "--repo",
                "/workspace/demo/demo",
                "--branch",
                "ws/feat/example",
            ]
        )

        self.assertEqual("finish-handoff", args.command)
        self.assertFalse(hasattr(args, "continuation"))

    def test_direct_mode_preserves_exact_request_without_handoff(self) -> None:
        worktree = self.fixture.worktree()
        request = "Fix the exact behavior.\nKeep this second line."

        result = lifecycle.reconcile_input(
            lifecycle.CommandRunner(),
            worktree,
            "direct",
            request,
            None,
            {},
        )

        self.assertEqual("direct", result["mode"])
        self.assertEqual(request, result["queuePayload"])
        self.assertIsNone(result["handoff"])
        self.assertFalse((worktree / "handoff.md").exists())

    def test_contextual_mode_materializes_and_reuses_exact_handoff(self) -> None:
        worktree = self.fixture.worktree()
        content = b"# Task handoff\n\n## Original request\nFix this contextual issue.\n"

        first = lifecycle.reconcile_input(
            lifecycle.CommandRunner(),
            worktree,
            "contextual",
            None,
            content,
            {},
        )
        second = lifecycle.reconcile_input(
            lifecycle.CommandRunner(),
            worktree,
            "contextual",
            None,
            content,
            first,
        )

        handoff = first["handoff"]
        self.assertEqual(str(worktree / "handoff.md"), handoff["path"])
        self.assertEqual("?? handoff.md", handoff["gitStatus"])
        self.assertIn(handoff["sha256"], first["queuePayload"])
        self.assertEqual(first, second)
        status = subprocess.run(
            ("git", "-C", str(worktree), "status", "--short"),
            text=True,
            stdout=subprocess.PIPE,
            check=True,
        ).stdout
        self.assertEqual("?? handoff.md\n", status)

    def test_contextual_mode_rejects_different_existing_handoff(self) -> None:
        worktree = self.fixture.worktree()
        (worktree / "handoff.md").write_text("existing\n")

        with self.assertRaises(lifecycle.LifecycleError) as caught:
            lifecycle.reconcile_input(
                lifecycle.CommandRunner(),
                worktree,
                "contextual",
                None,
                b"different\n",
                {},
            )

        self.assertEqual("handoff_conflict", caught.exception.code)
        self.assertEqual("existing\n", (worktree / "handoff.md").read_text())

    def test_contextual_mode_does_not_adopt_unrecorded_handoff(self) -> None:
        worktree = self.fixture.worktree()
        (worktree / "handoff.md").write_text("unowned\n")

        with self.assertRaises(lifecycle.LifecycleError) as caught:
            lifecycle.reconcile_input(
                lifecycle.CommandRunner(),
                worktree,
                "contextual",
                None,
                None,
                {},
            )

        self.assertEqual("invalid_input", caught.exception.code)

    def test_contextual_mode_rejects_empty_or_oversized_handoff(self) -> None:
        worktree = self.fixture.worktree()
        with self.assertRaises(lifecycle.LifecycleError) as empty:
            lifecycle.reconcile_input(
                lifecycle.CommandRunner(), worktree, "contextual", None, b"\n", {}
            )
        self.assertEqual("invalid_input", empty.exception.code)

        with self.assertRaises(lifecycle.LifecycleError) as oversized:
            lifecycle.reconcile_input(
                lifecycle.CommandRunner(),
                worktree,
                "contextual",
                None,
                b"x" * (lifecycle.MAX_HANDOFF_BYTES + 1),
                {},
            )
        self.assertEqual("handoff_too_large", oversized.exception.code)

    def test_untracked_inventory_includes_ignored_and_regular_files(self) -> None:
        worktree = self.fixture.worktree()
        (worktree / ".gitignore").write_text(".cache/\n")
        self.fixture.git_at(worktree, "add", ".gitignore")
        self.fixture.git_at(worktree, "commit", "-qm", "ignore cache")
        (worktree / "handoff.md").write_text("user work\n")
        (worktree / ".cache").mkdir()
        (worktree / ".cache" / "artifact").write_text("cache\n")

        paths = lifecycle.untracked_worktree_paths(
            lifecycle.CommandRunner(), worktree
        )

        self.assertEqual([".cache", "handoff.md"], paths)

    def test_tracked_status_excludes_untracked_files(self) -> None:
        worktree = self.fixture.worktree()
        tracked = worktree / "tracked.txt"
        tracked.write_text("initial\n")
        self.fixture.git_at(worktree, "add", "tracked.txt")
        self.fixture.git_at(worktree, "commit", "-qm", "add tracked file")
        tracked.write_text("changed\n")
        (worktree / "handoff.md").write_text("untracked\n")

        status = lifecycle.tracked_worktree_status(
            lifecycle.CommandRunner(), worktree
        )

        self.assertIn("tracked.txt", status)
        self.assertNotIn("handoff.md", status)

    def test_deferred_mode_leaves_queue_and_worktree_untouched(self) -> None:
        worktree = self.fixture.worktree()
        before = subprocess.run(
            ("git", "-C", str(worktree), "status", "--short"),
            text=True,
            stdout=subprocess.PIPE,
            check=True,
        ).stdout

        result = lifecycle.reconcile_input(
            lifecycle.CommandRunner(),
            worktree,
            "deferred",
            None,
            None,
            {},
        )

        self.assertEqual("deferred", result["mode"])
        self.assertIsNone(result["queuePayload"])
        self.assertIsNone(result["handoff"])
        self.assertEqual(before, subprocess.run(
            ("git", "-C", str(worktree), "status", "--short"),
            text=True,
            stdout=subprocess.PIPE,
            check=True,
        ).stdout)

    def test_parser_has_no_lifecycle_state_file(self) -> None:
        with self.assertRaises(SystemExit):
            lifecycle.build_parser().parse_args(
                [
                    "--state-file", "/tmp/state.json", "status",
                    "--repo", "/workspace/demo/demo",
                    "--branch", "ws/feat/example",
                ]
            )

    def test_finish_close_discovers_live_resources_and_archives_last(self) -> None:
        args = type("Args", (), {
            "workspace_root": str(self.fixture.workspace),
            "repo": str(self.fixture.repo),
            "branch": "ws/feat/example",
            "group_name": None,
            "codex_control": str(Path(__file__).resolve().parents[1] / "scripts" / "codex-task-control"),
            "codex_socket": None,
            "lark_cli": "lark-cli",
            "thread_id": "thread-exact",
        })()
        events = []

        def codex(*_args, **kwargs):
            if _args[2] == "inspect":
                return {
                    "threadId": "thread-exact",
                    "name": "ws/feat/example",
                    "cwd": str(
                        self.fixture.workspace
                        / "demo"
                        / "worktrees"
                        / "ws_feat_example"
                    ),
                    "ephemeral": False,
                    "status": "active",
                    "verified": True,
                }
            self.assertEqual("archive", _args[2])
            events.append("archive")
            return {
                "verified": True,
                "archived": False,
                "scheduled": True,
                "stateFile": "/tmp/archive-state.json",
            }

        with mock.patch.object(lifecycle, "discover_group", return_value={"chatId": "oc_test"}):
            with mock.patch.object(
                lifecycle,
                "remove_feed_shortcut",
                side_effect=lambda *_args: events.append("shortcut") or {
                    "present": False,
                    "verified": True,
                },
            ):
                with mock.patch.object(lifecycle, "codex_command", side_effect=codex):
                    result = lifecycle.finish_close_task(args, lifecycle.CommandRunner())

        self.assertTrue(result["verified"])
        self.assertEqual("archive", events[-1])
        self.assertNotIn("stateFile", result)
        self.assertEqual(["shortcut", "archive"], events)


if __name__ == "__main__":
    unittest.main()
