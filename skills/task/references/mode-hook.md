# Task collaboration-mode hook

The Task skill uses a global Codex `UserPromptSubmit` hook so an explicit task
transition is handled before the Agent sees the prompt. This avoids asking a
Plan-mode Agent to perform the action that ends Plan mode.

Users can submit any of these complete commands, ignoring surrounding whitespace
and case. The primitive may be any unique ASCII prefix of at least three letters:

```text
$task exe
$task execute
$task fin
$task finish
```

Every prefix from `exe` through `execute` normalizes to `execute`, and every prefix
from `fin` through `finish` normalizes to `finish`. Shorter prefixes, non-prefix
words, extra arguments, and surrounding prose do not trigger the hook.

Native Codex and cc-connect both submit the canonical `$task ...` command as written,
so the hook sees the same raw transition prompt from the terminal or Feishu.
cc-connect's legacy `/task ...` alias instead expands into a structured skill
invocation before calling Codex. The hook retains compatibility with that envelope
only when its skill is exactly `task`, its final user-argument block is one valid
transition primitive, and its standard trailer is intact. It also retains raw
`/task ...` recognition for older clients. Text that merely mentions either form
inside the Skill body does not trigger the hook. Keep cc-connect project
`inject_sender = false`; sender-prefixed prompts and envelopes intentionally fail
closed.

For every accepted transition prompt, the hook:

1. reads the exact native thread ID from the hook event's `session_id`;
2. runs the sibling `scripts/codex-task-control --thread <id> --json default`;
3. validates the thread ID, Default mode, and verified receipt;
4. adds explicit Default-mode developer context for the current turn.

The App Server update is idempotent and makes Default persistent for subsequent
turns. The developer context ends Plan mode for the prompt already being submitted.
Do not use the hook event's `permission_mode` to detect collaboration mode: in Codex
0.152.x that field is derived from approval policy and a Plan thread can report
`bypassPermissions`. The hook therefore performs the verified Default update for
every accepted transition prompt, even when the thread is already in Default mode.
It does nothing only when the prompt is not an accepted transition. A missing ID,
unreachable App Server, timeout, or unverified receipt blocks the prompt before it
reaches the Agent.

Codex loads the hook from `~/.codex/hooks.json`. A newly installed or changed
non-managed hook must be reviewed and trusted once through `/hooks`; until then,
Codex skips it. If an explicit execute or finish request still reaches the Agent in
Plan mode, stop and report that the global hook is missing, disabled, untrusted,
unable to reach the local App Server, or no longer recognizes the submitted Task
prompt. Do not fall back to a model-driven mode handoff.

For an isolated App Server test, set `CODEX_TASK_CONTROL_SOCKET` to the test Unix
socket before starting Codex. Production normally uses the default managed App
Server control socket.
