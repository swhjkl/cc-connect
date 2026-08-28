# Task lifecycle control CLI

Local task automation controls cc-connect through its owner-only Unix socket.
Every command requires an explicit project, platform session key, and JSON
output. Responses use `schema_version: 1` and contain either `result` or a
stable `error.code`.

The envelope is identical for workspace and sessions operations:
`{"schema_version":1,"ok":true,"result":{...}}` on success, or
`{"schema_version":1,"ok":false,"error":{"code":"...","message":"..."}}`
on failure. `closeout_guard` is nested under `result` only for workspace
unbind responses.

```sh
cc-connect workspace status --project <project> --session <platform:chat:user> --json
cc-connect workspace route --project <project> --session <platform:chat:user> \
  --worktree /absolute/existing/worktree --json
cc-connect sessions status --project <project> --session <platform:chat:user> --json
cc-connect sessions attach --project <project> --session <platform:chat:user> \
  --agent-session-id <exact-native-agent-session-id> --json
```

Routing is idempotent: requesting the exact existing project worktree returns
`status: "already_routed"` and `changed: false`, including while that task's
session is busy. Busy protection still rejects new or different routing.

Task closeout must use the compare-and-set form below. The native ID is the
full agent thread/session ID; prefixes and display names are not accepted.

```sh
cc-connect workspace unbind --project <project> --session <platform:chat:user> \
  --worktree /absolute/existing/worktree \
  --expected-agent-session-id <exact-native-agent-session-id> --json
```

If the chat session is busy, unbind succeeds only when the routed worktree,
the active internal session's native ID, the live agent process's current ID,
and any existing conversation mirror all identify that exact task thread. The
operation removes only the project route; it does not close, cancel, switch,
archive, or otherwise mutate the active turn. A successful busy response
contains:

```json
{
  "schema_version": 1,
  "ok": true,
  "result": {
    "status": "unbound",
    "changed": true,
    "closeout_guard": {
      "expected_agent_session_id": "<id>",
      "active_agent_session_id": "<id>",
      "live_agent_session_id": "<id>",
      "busy": true,
      "verified": true,
      "active_turn_preserved": true
    }
  }
}
```

Mismatched or unrelated busy sessions return `state_conflict` and retain the
route. Exit codes are: 0 success/idempotent, 1 internal/persistence failure,
2 invalid arguments, 3 not found, 4 state conflict, and 5 daemon/socket/schema
unavailable.
