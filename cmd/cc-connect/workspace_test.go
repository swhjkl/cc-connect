package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWorkspaceUnbindRequiresExactCloseoutGuard(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runWorkspaceCLI([]string{"unbind", "--project", "p", "--session", "feishu:c:u", "--worktree", "/tmp", "--json"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--expected-agent-session-id is required") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestLifecycleSessionsAttachRequiresExactNativeID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runLifecycleSessionsCLI([]string{"attach", "--project", "p", "--session", "feishu:c:u", "--json"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--agent-session-id is required") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}
