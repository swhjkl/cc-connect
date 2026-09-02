#!/usr/bin/env python3

from __future__ import annotations

import importlib.machinery
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest
from unittest import mock


HELPER = Path(__file__).resolve().parents[1] / "scripts" / "task-user-prompt-submit"


def load_helper():
    name = "task_user_prompt_submit_test"
    loader = importlib.machinery.SourceFileLoader(name, str(HELPER))
    spec = importlib.util.spec_from_loader(name, loader)
    if spec is None:
        raise RuntimeError("could not load task-user-prompt-submit")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    loader.exec_module(module)
    return module


hook = load_helper()


def completed_receipt(thread_id: str = "thread-1", **overrides):
    receipt = {
        "action": "mode",
        "threadId": thread_id,
        "mode": "default",
        "verification": "notification",
        "verified": True,
    }
    receipt.update(overrides)
    return subprocess.CompletedProcess(
        args=[], returncode=0, stdout=json.dumps(receipt), stderr=""
    )


def cc_connect_envelope(argument: str, skill: str = "task") -> str:
    return (
        "The user is asking you to execute the following skill.\n\n"
        f"## Skill: {skill}\n"
        "## Description: Manage an explicit task lifecycle.\n\n"
        "## Skill Instructions:\n"
        "The instructions may mention $task execute without triggering the hook.\n\n"
        "## User Arguments:\n"
        f"{argument}\n\n"
        "Please follow the skill instructions above to complete the task."
    )


class TaskUserPromptSubmitTest(unittest.TestCase):
    def test_parser_accepts_unique_primitive_prefixes_of_at_least_three_letters(self):
        accepted = {
            "$task exe": "execute",
            "$task exec": "execute",
            "$task execu": "execute",
            "$task execut": "execute",
            "$task execute": "execute",
            "$task fin": "finish",
            "$task fini": "finish",
            "$task finis": "finish",
            " $task finish\n": "finish",
            "$TASK   EXE": "execute",
        }
        for prompt, expected in accepted.items():
            with self.subTest(prompt=prompt):
                self.assertEqual(expected, hook.transition_primitive(prompt))

        rejected = (
            "$task e",
            "$task ex",
            "$task f",
            "$task fi",
            "$task execution",
            "$task executed",
            "$task final",
            "$task finished",
            "$task init fix this",
            "$task execute now",
            "$task exe now",
            "please run $task execute",
            "`$task finish`",
            "task execute",
            None,
        )
        for prompt in rejected:
            with self.subTest(prompt=prompt):
                self.assertIsNone(hook.transition_primitive(prompt))

    def test_parser_retains_legacy_slash_command_compatibility(self):
        accepted = {
            "/task exe": "execute",
            "/task execute": "execute",
            "/task fin": "finish",
            " /task finish\n": "finish",
        }
        for prompt, expected in accepted.items():
            with self.subTest(prompt=prompt):
                self.assertEqual(expected, hook.transition_primitive(prompt))

    def test_parser_accepts_legacy_cc_connect_task_envelope(self):
        accepted = {
            cc_connect_envelope("exe"): "execute",
            cc_connect_envelope("EXECUTE"): "execute",
            cc_connect_envelope("fin"): "finish",
            cc_connect_envelope("finish"): "finish",
        }
        for prompt, expected in accepted.items():
            with self.subTest(argument=expected):
                self.assertEqual(expected, hook.transition_primitive(prompt))

    def test_parser_rejects_noncanonical_legacy_cc_connect_envelopes(self):
        valid = cc_connect_envelope("execute")
        rejected = (
            cc_connect_envelope("execute now"),
            cc_connect_envelope("$task execute"),
            cc_connect_envelope("execute", skill="other"),
            valid.removesuffix(
                "Please follow the skill instructions above to complete the task."
            ),
            valid.replace("## Skill Instructions:", "## Instructions:"),
            "prefix\n" + valid,
        )
        for prompt in rejected:
            with self.subTest(prompt=prompt[-80:]):
                self.assertIsNone(hook.transition_primitive(prompt))

    def test_non_transition_event_does_not_call_controller(self):
        runner = mock.Mock(side_effect=AssertionError("controller must not run"))
        result = hook.process_event(
            {
                "session_id": "thread-1",
                "permission_mode": "default",
                "prompt": "$task init investigate this",
            },
            runner=runner,
        )

        self.assertIsNone(result)
        runner.assert_not_called()

    def test_execute_ignores_permission_mode_and_injects_default_context(self):
        runner = mock.Mock(return_value=completed_receipt())
        result = hook.process_event(
            {
                "session_id": "thread-1",
                # Codex 0.152 derives this field from approval policy, not
                # collaboration mode. Task threads commonly report this value.
                "permission_mode": "bypassPermissions",
                "prompt": "$task exe",
            },
            runner=runner,
        )

        self.assertIsNotNone(result)
        context = result["hookSpecificOutput"]["additionalContext"]
        self.assertIn("<collaboration_mode>", context)
        self.assertIn("# Collaboration Mode: Default", context)
        self.assertIn("primitive=execute", context)
        command = runner.call_args.args[0]
        self.assertEqual(str(hook.CONTROL), command[0])
        self.assertEqual(
            ["--thread", "thread-1", "--json", "default"], command[1:]
        )

    def test_legacy_cc_connect_envelope_runs_same_transition(self):
        runner = mock.Mock(return_value=completed_receipt())
        result = hook.process_event(
            {
                "session_id": "thread-1",
                "permission_mode": "bypassPermissions",
                "prompt": cc_connect_envelope("execute"),
            },
            runner=runner,
        )

        self.assertIsNotNone(result)
        self.assertIn(
            "primitive=execute",
            result["hookSpecificOutput"]["additionalContext"],
        )
        runner.assert_called_once()

    def test_socket_override_is_passed_to_controller(self):
        runner = mock.Mock(return_value=completed_receipt())
        with mock.patch.dict(
            os.environ,
            {"CODEX_TASK_CONTROL_SOCKET": "/tmp/test-app-server.sock"},
        ):
            hook.process_event(
                {
                    "session_id": "thread-1",
                    "permission_mode": "plan",
                    "prompt": "$task fin",
                },
                runner=runner,
            )

        self.assertEqual(
            [
                str(hook.CONTROL),
                "--thread",
                "thread-1",
                "--socket",
                "/tmp/test-app-server.sock",
                "--json",
                "default",
            ],
            runner.call_args.args[0],
        )

    def test_missing_session_id_blocks_transition_prompt(self):
        result = hook.process_event(
            {"permission_mode": "plan", "prompt": "$task finish"}
        )

        self.assertEqual("block", result["decision"])
        self.assertIn("session_id", result["reason"])

    def test_unverified_or_wrong_thread_receipt_blocks_transition_prompt(self):
        for receipt in (
            completed_receipt(verified=False),
            completed_receipt(thread_id="other-thread"),
            completed_receipt(mode="plan"),
        ):
            with self.subTest(stdout=receipt.stdout):
                result = hook.process_event(
                    {
                        "session_id": "thread-1",
                        "permission_mode": "plan",
                        "prompt": "$task execute",
                    },
                    runner=mock.Mock(return_value=receipt),
                )
                self.assertEqual("block", result["decision"])
                self.assertIn("failed verification", result["reason"])

    def test_controller_failure_blocks_without_exposing_unbounded_output(self):
        runner = mock.Mock(
            return_value=subprocess.CompletedProcess(
                args=[], returncode=1, stdout="", stderr="failure " * 200
            )
        )
        result = hook.process_event(
            {
                "session_id": "thread-1",
                "permission_mode": "plan",
                "prompt": "$task finish",
            },
            runner=runner,
        )

        self.assertEqual("block", result["decision"])
        self.assertLess(len(result["reason"]), 750)

    def test_controller_timeout_blocks_transition_prompt(self):
        runner = mock.Mock(
            side_effect=subprocess.TimeoutExpired(cmd=["control"], timeout=15)
        )
        result = hook.process_event(
            {
                "session_id": "thread-1",
                "permission_mode": "plan",
                "prompt": "$task execute",
            },
            runner=runner,
        )

        self.assertEqual("block", result["decision"])
        self.assertIn("timed out", result["reason"])


if __name__ == "__main__":
    unittest.main()
