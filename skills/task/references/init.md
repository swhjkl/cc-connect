# Task initialization

Use this reference for explicit `init` only. `execute` and `finish` do not depend on init having run or on a recorded lifecycle phase.

Resolve the canonical stable repository, exact `ws/<type>/<slug>` branch, optional base ref, and input mode. Then call `scripts/task-lifecycle-control init`. The command reconciles each resource directly from its owner:

- Git is authoritative for the branch and canonical linked worktree.
- Codex is authoritative for the persistent exact-name, exact-cwd native thread.
- Lark is authoritative for the exact-name group, membership, native-session-ID
  description, and Feed Shortcut.
- cc-connect is authoritative for the project route and native agent-session attachment.

No lifecycle state file or event log is created or consulted. A newly created group
stores the exact native Codex session ID as its description in the create operation;
re-running init reads and repairs that description before cc-connect is attached.
Re-running init is idempotent only when the live identities match exactly; ambiguous
or conflicting resources stop the operation.

Direct mode queues the exact effective request. Contextual mode writes only the supplied immutable `handoff.md`, verifies its bytes and Git status, and queues its absolute path and SHA-256. Deferred mode does not queue a prompt. Init switches subsequent turns to Plan mode and does not implement.

Never send lifecycle controls through Feishu. Use the local cc-connect control CLI for route, attach, status, and unbind.
