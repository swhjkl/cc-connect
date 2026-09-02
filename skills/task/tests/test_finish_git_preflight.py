#!/usr/bin/env python3

from __future__ import annotations

import json
from pathlib import Path
import subprocess
import tempfile
import unittest


HELPER = Path(__file__).resolve().parents[1] / "scripts" / "task-finish-git-preflight"


class RepoFixture:
    def __init__(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="task-finish-preflight-")
        self.root = Path(self.temp.name)
        self.git("init", "-q", "-b", "stable")
        self.git("config", "user.email", "test@example.com")
        self.git("config", "user.name", "Task Skill Test")
        self.write(".gitignore", "ignored.txt\nignored-node\n")
        self.write("tracked.txt", "base\n")
        self.write("staged.txt", "base\n")
        self.git("add", ".")
        self.git("commit", "-qm", "base")
        self.base = self.rev("HEAD")

    def close(self) -> None:
        self.temp.cleanup()

    def git(self, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["git", "-C", str(self.root), *args],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=check,
        )

    def write(self, relative: str, content: str) -> None:
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    def rev(self, ref: str) -> str:
        return self.git("rev-parse", ref).stdout.strip()

    def target(self, changes: dict[str, str], force_add: tuple[str, ...] = ()) -> str:
        self.git("switch", "-q", "-c", "target")
        for relative, content in changes.items():
            self.write(relative, content)
        self.git("add", ".")
        if force_add:
            self.git("add", "-f", *force_add)
        self.git("commit", "-qm", "target")
        target = self.rev("HEAD")
        self.git("switch", "-q", "stable")
        return target

    def check(self, target: str) -> tuple[int, dict[str, object]]:
        result = subprocess.run(
            [str(HELPER), "--stable", str(self.root), "--target", target, "--json"],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        return result.returncode, json.loads(result.stdout)


class FinishGitPreflightTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = RepoFixture()

    def tearDown(self) -> None:
        self.repo.close()

    def test_up_to_date_allows_all_dirty_kinds(self) -> None:
        self.repo.write("tracked.txt", "unstaged\n")
        self.repo.write("staged.txt", "staged\n")
        self.repo.git("add", "staged.txt")
        self.repo.write("untracked.txt", "keep\n")
        self.repo.write("ignored.txt", "keep\n")
        before = self.repo.git("status", "--short", "--ignored").stdout

        code, payload = self.repo.check(self.repo.base)

        self.assertEqual(0, code)
        self.assertTrue(payload["safe"])
        self.assertFalse(payload["needsFastForward"])
        self.assertEqual(before, self.repo.git("status", "--short", "--ignored").stdout)

    def test_fast_forward_allows_unrelated_dirty_state(self) -> None:
        target = self.repo.target({"target.txt": "target\n"})
        self.repo.write("tracked.txt", "unstaged\n")
        self.repo.write("staged.txt", "staged\n")
        self.repo.git("add", "staged.txt")
        self.repo.write("untracked.txt", "keep\n")
        self.repo.write("ignored.txt", "keep\n")
        before = self.repo.git("status", "--short", "--ignored").stdout

        code, payload = self.repo.check(target)

        self.assertEqual(0, code)
        self.assertTrue(payload["safe"])
        self.assertTrue(payload["needsFastForward"])
        self.assertEqual(before, self.repo.git("status", "--short", "--ignored").stdout)

        self.repo.git("merge", "--ff-only", target)

        self.assertEqual(target, self.repo.rev("HEAD"))
        self.assertEqual("target\n", (self.repo.root / "target.txt").read_text())
        self.assertEqual("unstaged\n", (self.repo.root / "tracked.txt").read_text())
        self.assertEqual("staged\n", (self.repo.root / "staged.txt").read_text())
        self.assertEqual("keep\n", (self.repo.root / "untracked.txt").read_text())
        self.assertEqual("keep\n", (self.repo.root / "ignored.txt").read_text())
        self.assertEqual(before, self.repo.git("status", "--short", "--ignored").stdout)

    def test_rejects_tracked_update_conflict(self) -> None:
        target = self.repo.target({"tracked.txt": "target\n"})
        self.repo.write("tracked.txt", "local\n")

        code, payload = self.repo.check(target)

        self.assertEqual(1, code)
        self.assertEqual("checkout_conflict", payload["reason"])
        self.assertEqual("local\n", (self.repo.root / "tracked.txt").read_text())

    def test_rejects_untracked_exact_and_directory_conflicts(self) -> None:
        target = self.repo.target({"new.txt": "target\n", "dir/file.txt": "target\n"})
        self.repo.write("new.txt", "local\n")

        code, payload = self.repo.check(target)
        self.assertEqual(1, code)
        self.assertEqual("checkout_conflict", payload["reason"])

        (self.repo.root / "new.txt").unlink()
        self.repo.write("dir", "local\n")
        code, payload = self.repo.check(target)
        self.assertEqual(1, code)
        self.assertEqual("checkout_conflict", payload["reason"])

    def test_rejects_ignored_exact_and_directory_conflicts(self) -> None:
        target = self.repo.target(
            {"ignored.txt": "target\n", "ignored-node": "target\n"},
            force_add=("ignored.txt", "ignored-node"),
        )
        self.repo.write("ignored.txt", "local\n")

        code, payload = self.repo.check(target)
        self.assertEqual(1, code)
        self.assertEqual("ignored_path_collision", payload["reason"])
        self.assertIn("ignored.txt", payload["details"]["paths"])

        (self.repo.root / "ignored.txt").unlink()
        self.repo.write("ignored-node/local.txt", "local\n")
        code, payload = self.repo.check(target)
        self.assertEqual(1, code)
        self.assertEqual("ignored_path_collision", payload["reason"])
        self.assertIn("ignored-node/local.txt", payload["details"]["paths"])

    def test_rejects_non_fast_forward(self) -> None:
        target = self.repo.target({"target.txt": "target\n"})
        self.repo.write("stable.txt", "stable\n")
        self.repo.git("add", "stable.txt")
        self.repo.git("commit", "-qm", "stable divergence")

        code, payload = self.repo.check(target)

        self.assertEqual(1, code)
        self.assertEqual("not_fast_forward", payload["reason"])

    def test_rejects_git_operation_even_when_up_to_date(self) -> None:
        merge_head = Path(self.repo.git("rev-parse", "--git-path", "MERGE_HEAD").stdout.strip())
        if not merge_head.is_absolute():
            merge_head = self.repo.root / merge_head
        merge_head.write_text(self.repo.base + "\n")

        code, payload = self.repo.check(self.repo.base)

        self.assertEqual(1, code)
        self.assertEqual("git_operation_in_progress", payload["reason"])


if __name__ == "__main__":
    unittest.main()
