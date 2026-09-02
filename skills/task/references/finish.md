# Task finish

Explicit `finish` means the user wants to end this workspace/thread now.
Task-worktree cleanup still requires no tracked/staged edits and proof that every
committed task change is already present in the stable default branch; finish never
creates a commit or integrates task changes on the user's behalf. Stable-checkout
cleanup instead preserves every local file and has no branch integration gate.

## Collaboration-mode handoff

The global `UserPromptSubmit` hook handles a complete `$task fin` through
`$task finish` before the Agent turn, whether it came from native Codex or Feishu via
cc-connect. The hook also recognizes cc-connect's generated task-skill envelope for
the legacy `/task ...` alias. When the thread was in Plan mode, require the injected
developer context confirming the verified Default transition, then continue cleanup
in the same turn. Do not call `finish-handoff` or create a queued continuation on
the normal path.

If the explicit finish prompt still arrives with Plan instructions active, the hook
was missing, disabled, untrusted, unable to update the native thread, or unable to
recognize the submitted prompt. Stop before cleanup and report that control-plane
failure. A Plan-mode Agent must not invoke a script to escape its own mode, and the
user must not be asked to resubmit the same finish request manually.

## Cleanup

The hard safety rule is that cleanup must not lose uncommitted work. `task-lifecycle-control finish-cleanup` reads current facts from Git, Codex, Lark, and cc-connect. It never reads or writes a lifecycle state/log file.

Run:

```sh
~/.agents/skills/task/scripts/task-lifecycle-control --json finish-cleanup \
  --repo <canonical-stable-checkout> \
  <target-selector>
```

### Task-worktree target

With `--branch <exact-ws-branch>`, the command:

- derives and verifies the canonical worktree and branch from Git;
- blocks on staged or tracked changes;
- treats all untracked content, whether ignored or not, as recoverable local data: it moves that content into `<workspace>/<repo>/archive/<YYYY-MM>/<task-id>-<timestamp>/`, records a private manifest, then removes and verifies the linked worktree registration;
- publishes a local-content archive only after Git no longer lists the worktree; if worktree removal fails while the original directory remains, it rolls the staged content back;
- removes complete, lifecycle-owned archives once their archive month is three calendar months behind the current month (for example, an August archive first becomes eligible on November 1); malformed, incomplete, linked, or non-private archive entries are preserved;
- archives an untracked `handoff.md` like any other local file, without relying on its name or contents to infer ownership;
- first checks whether the local stable default branch contains the task HEAD or
  has the same final content at every path changed by the task, so squash and
  cherry-pick integration can pass without requiring identical commit IDs;
- only when the local check fails, fetches the remote default branch and safely
  fast-forwards the stable checkout after a read-only overwrite preflight; if the
  refreshed branch still lacks any task change, stops before notifications,
  unbinding, handoff removal, or worktree removal;
- discovers the exact-name native thread when present; otherwise verifies the currently invoking active thread by ID and exact cwd, and additionally requires that ID to match the cc-connect session attachment whenever a route exists;
- rejects every unrelated active thread using the worktree;
- reads the exact cc-connect route/session and uses the native thread ID as the compare-and-set guard when unbinding, including safe closeout from the current busy finishing turn;
- removes the linked worktree without force, verifies that the archive has no worktree relationship, and preserves the branch and commits;
- never commits, merges, rebases, or pushes the task branch.

### Stable-checkout target

Use `--stable-checkout` only when the invoking native thread's cwd is exactly the canonical stable checkout. This mode completes lifecycle closeout except for every operation that deletes, archives, or purges a local directory:

- requires `CODEX_THREAD_ID` to identify the active invoking persistent native thread and verifies that thread by exact ID and stable-checkout cwd;
- preserves the canonical stable checkout directory and every tracked, staged, untracked, ignored, or otherwise local file without requiring a clean Git status;
- does not derive or validate a `ws/*` branch, inspect integration state, fetch or update the stable checkout, select another linked worktree, archive local content, remove a worktree, or purge old archive directories;
- reverse-resolves at most one active cc-connect session whose attached native ID is the invoking thread, then verifies its route and session through the lifecycle API;
- when that exact session routes to the stable checkout, unbinds it with the native thread ID as the compare-and-set guard, including the verified busy-turn closeout guard;
- resolves a corresponding Feishu group only from that exact attachment; a supplied `--group-name` is an additional identity check, never an independent group selector;
- leaves any unrelated task worktree, route, group, shortcut, branch, thread, and archive untouched.

If no exact active cc-connect attachment exists, route and Feed Shortcut cleanup are not applicable; finish still closes the verified current native thread. Multiple matching attachments, a mismatched route/thread, or a group-name mismatch are conflicts and stop before mutation.

A task record is optional. If the caller supplies `--record-url`, it must also supply `--document-ready`; the script validates and sends the canonical notification before unbinding. Missing documents never block finish.

After cleanup, prepare the final response. Then run `finish-close` as the final external operation:

```sh
~/.agents/skills/task/scripts/task-lifecycle-control --json finish-close \
  --repo <canonical-stable-checkout> \
  <same-target-selector> \
  --thread-id <finish-cleanup.closeThreadId> \
  [--session-key <finish-cleanup.closeSessionKey>]
```

When cleanup returns a non-empty `closeThreadId`, pass it exactly as shown. In task-worktree mode, the command re-verifies that native thread by ID and task-worktree cwd even after Git removed the directory; a legacy name is accepted only for the currently invoking native thread. If task-worktree cleanup returns `null` because no native task thread exists, omit `--thread-id`. In stable-checkout mode, `closeThreadId` is always the invoking thread ID and any supplied value must match it; when `closeSessionKey` is non-empty, pass it exactly with `--session-key` so a changed attachment stops closeout. The command re-resolves the exact attached group, removes its Feed Shortcut when present, and archives only the verified current thread. Missing already-cleaned resources are idempotent. Native archival is the final external operation; after it, perform no tool, filesystem, document, or messaging operation.

Never force-remove a dirty worktree, delete the task branch, overwrite commits, remove a stable checkout, or unbind a mismatched/busy unrelated session. The outer `archive/` directory is lifecycle-owned, mode `0700`, and identified by `.task-archive.json`; do not adopt or purge an unrelated directory with the same name. Fix the reported source-of-truth conflict and retry.
