package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestAppServerMethodUnavailable(t *testing.T) {
	for _, test := range []struct {
		message string
		want    bool
	}{
		{message: "Method not found", want: true},
		{message: "unknown method thread/turns/list", want: true},
		{message: "thread/turns/list timed out", want: false},
		{message: "connection closed", want: false},
	} {
		if got := appServerMethodUnavailable(errors.New(test.message)); got != test.want {
			t.Fatalf("appServerMethodUnavailable(%q) = %v, want %v", test.message, got, test.want)
		}
	}
}

func TestAppServerConversationPresentationEvents_MatchesLiveEventSemantics(t *testing.T) {
	items := []map[string]any{
		{"type": "agentMessage", "id": "commentary-1", "text": "Inspecting files"},
		{
			"type": "mcpToolCall", "id": "tool-1", "server": "workspace", "tool": "read_file",
			"arguments": map[string]any{"path": "README.md"}, "result": map[string]any{"text": "contents"}, "status": "completed",
		},
		{"type": "reasoning", "id": "reasoning-1", "summary": []any{"Checking the result"}},
		{"type": "agentMessage", "id": "answer-1", "text": "Done"},
	}

	events := appServerConversationPresentationEvents(items, core.ConversationTurnCompleted)
	if len(events) != 5 {
		t.Fatalf("presentation events = %#v, want thinking, tool use/result, thinking, text", events)
	}
	wantTypes := []core.EventType{
		core.EventThinking, core.EventToolUse, core.EventToolResult, core.EventThinking, core.EventText,
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event[%d].Type = %q, want %q; events=%#v", i, events[i].Type, want, events)
		}
	}
	if events[1].ItemID != "tool-1" || events[1].ToolName != "MCP" || !strings.Contains(events[1].ToolInput, "workspace:read_file") {
		t.Fatalf("tool-use event = %#v", events[1])
	}
	if events[2].ItemID != "tool-1" || events[2].ToolName != "read_file" || !strings.Contains(events[2].ToolResult, "contents") {
		t.Fatalf("tool-result event = %#v", events[2])
	}
	if events[4].Content != "Done" {
		t.Fatalf("final text event = %#v", events[4])
	}
}

func TestAgentGetConversation_ReadsDaemonWithoutResumingThread(t *testing.T) {
	daemon := newFakeSharedAppServerDaemon(t)
	workDir := t.TempDir()
	threadID := "thread-observed"
	daemon.setConversation(workDir, threadID, "active", []string{"waitingOnUserInput"}, []map[string]any{
		{
			"id": "turn-new", "status": "inProgress", "startedAt": int64(200), "completedAt": nil,
			"items": []any{
				map[string]any{"type": "userMessage", "id": "u-new", "clientId": "message-marker", "content": []any{map[string]any{"type": "text", "text": "latest prompt"}}},
				map[string]any{"type": "agentMessage", "id": "a-new", "text": "Working on it.", "phase": "commentary"},
				map[string]any{"type": "commandExecution", "id": "tool-new", "command": "cat /secret/path", "status": "inProgress"},
				map[string]any{"type": "reasoning", "id": "r-new", "summary": []any{"private reasoning"}},
			},
		},
		{
			"id": "turn-old", "status": "completed", "startedAt": int64(100), "completedAt": int64(110),
			"items": []any{
				map[string]any{
					"type": "userMessage", "id": "u-old",
					"content": []any{map[string]any{
						"type": "text",
						"text": "Before answering, follow these project-level instructions for this cc-connect session. They are not user content.\n\nsecret preamble\n\n---\n\nUser message:\nold prompt",
					}},
				},
				map[string]any{"type": "agentMessage", "id": "a-old-commentary", "text": "Old commentary.", "phase": "commentary"},
				map[string]any{"type": "agentMessage", "id": "a-old-final", "text": "Old final.", "phase": "final_answer"},
				map[string]any{"type": "plan", "id": "plan-old", "text": "private plan detail"},
			},
		},
	})

	agent := &Agent{
		backend:            "app_server",
		appServerTransport: appServerTransportDaemon,
		appServerSocket:    daemon.socketPath,
		workDir:            workDir,
	}
	t.Cleanup(func() { _ = agent.Stop() })

	snapshot, err := agent.GetConversation(context.Background(), threadID, 2)
	if err != nil {
		t.Fatalf("GetConversation() error = %v", err)
	}
	if snapshot.SessionID != threadID || snapshot.ThreadState != "active" {
		t.Fatalf("snapshot identity/status = %#v", snapshot)
	}
	if len(snapshot.ActiveFlags) != 1 || snapshot.ActiveFlags[0] != "waitingOnUserInput" {
		t.Fatalf("active flags = %#v", snapshot.ActiveFlags)
	}
	if len(snapshot.Turns) != 2 || snapshot.Turns[0].ID != "turn-old" || snapshot.Turns[1].ID != "turn-new" {
		t.Fatalf("turn order = %#v", snapshot.Turns)
	}
	if got := snapshot.Turns[0].Messages[0].Content; got != "old prompt" {
		t.Fatalf("stripped prompt = %q, want old prompt", got)
	}
	if got := snapshot.Turns[0].Activities; len(got) != 1 || got[0].Kind != "plan" || got[0].Status != "completed" {
		t.Fatalf("completed status-less activity = %#v", got)
	}
	latest := snapshot.Turns[1]
	if latest.Status != "in_progress" || len(latest.Activities) != 1 {
		t.Fatalf("latest turn = %#v", latest)
	}
	if latest.Activities[0].Kind != "shell" || latest.Activities[0].Status != "in_progress" {
		t.Fatalf("latest activity = %#v", latest.Activities[0])
	}
	if got := latest.Messages[0].ClientID; got != "message-marker" {
		t.Fatalf("user message client marker = %q, want message-marker", got)
	}
	presentation := latest.PresentationEvents
	if len(presentation) != 3 {
		t.Fatalf("latest presentation events = %#v, want thinking, tool use, thinking", presentation)
	}
	if presentation[0].Type != core.EventThinking || presentation[0].Content != "Working on it." {
		t.Fatalf("commentary presentation = %#v", presentation[0])
	}
	if presentation[1].Type != core.EventToolUse || presentation[1].ItemID != "tool-new" || presentation[1].ToolName != "Bash" || presentation[1].ToolInput != "cat /secret/path" {
		t.Fatalf("tool presentation = %#v", presentation[1])
	}
	if presentation[2].Type != core.EventThinking || presentation[2].ItemID != "r-new" || presentation[2].Content != "private reasoning" {
		t.Fatalf("reasoning presentation = %#v", presentation[2])
	}
	serialized := strings.Join([]string{
		snapshot.Turns[0].Activities[0].Kind,
		snapshot.Turns[0].Activities[0].Status,
		latest.Messages[0].Content,
		latest.Messages[1].Content,
		latest.Activities[0].Kind,
		latest.Activities[0].Status,
	}, "\n")
	for _, forbidden := range []string{"cat /secret/path", "private reasoning", "private plan detail", "secret preamble"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("snapshot leaked %q: %q", forbidden, serialized)
		}
	}

	daemon.mu.Lock()
	clients := append([]*fakeSharedAppServerClient(nil), daemon.clients...)
	daemon.mu.Unlock()
	if len(clients) != 1 {
		t.Fatalf("daemon clients = %d, want one cached snapshot client", len(clients))
	}
	clients[0].daemon.mu.Lock()
	boundThread := clients[0].threadID
	clients[0].daemon.mu.Unlock()
	if boundThread != "" {
		t.Fatalf("snapshot client resumed/bound thread %q", boundThread)
	}
}

func TestAgentInterruptConversationTurn_UsesExactIDs(t *testing.T) {
	daemon := newFakeSharedAppServerDaemon(t)
	workDir := t.TempDir()
	agent := &Agent{
		backend:            "app_server",
		appServerTransport: appServerTransportDaemon,
		appServerSocket:    daemon.socketPath,
		workDir:            workDir,
	}
	t.Cleanup(func() { _ = agent.Stop() })

	if err := agent.InterruptConversationTurn(context.Background(), "thread-exact", "turn-exact"); err != nil {
		t.Fatalf("InterruptConversationTurn() error = %v", err)
	}
	select {
	case got := <-daemon.interrupts:
		if got.ThreadID != "thread-exact" || got.TurnID != "turn-exact" {
			t.Fatalf("interrupt ids = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for turn/interrupt")
	}
}

func TestAgentWatchConversation_IsReadOnlyAndFiltersThreadEvents(t *testing.T) {
	daemon := newFakeSharedAppServerDaemon(t)
	workDir := t.TempDir()
	threadID := "thread-observed"
	daemon.setConversation(workDir, threadID, "active", nil, []map[string]any{{
		"id": "turn-live", "status": "inProgress", "items": []any{},
	}})
	agent := &Agent{
		backend: "app_server", appServerTransport: appServerTransportDaemon,
		appServerSocket: daemon.socketPath, workDir: workDir,
	}
	t.Cleanup(func() { _ = agent.Stop() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := agent.WatchConversation(ctx, threadID)
	if err != nil {
		t.Fatalf("WatchConversation() error = %v", err)
	}
	daemon.waitForClients(t, 1)

	daemon.mu.Lock()
	clients := append([]*fakeSharedAppServerClient(nil), daemon.clients...)
	resumeCalls := clients[0].resumeCalls
	daemon.mu.Unlock()
	if resumeCalls != 0 {
		t.Fatalf("observer issued %d thread/resume calls", resumeCalls)
	}

	daemon.broadcast(t, "item/commandExecution/outputDelta", map[string]any{
		"threadId": "thread-other", "turnId": "turn-other", "itemId": "item-other", "delta": "secret",
	})
	daemon.broadcast(t, "item/commandExecution/outputDelta", map[string]any{
		"threadId": threadID, "turnId": "turn-live", "itemId": "item-live", "delta": "progress",
	})
	select {
	case event := <-events:
		if event.Type != core.EventConversationChanged || event.ThreadID != threadID || event.TurnID != "turn-live" || event.ItemID != "item-live" {
			t.Fatalf("observer event = %#v", event)
		}
		if event.Content != "" || event.ToolResult != "" {
			t.Fatalf("observer leaked raw delta content: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for identified observer event")
	}
}

func TestAgentSteerConversationTurn_UsesExpectedTurnWithoutResume(t *testing.T) {
	daemon := newFakeSharedAppServerDaemon(t)
	workDir := t.TempDir()
	daemon.setConversation(workDir, "thread-exact", "active", nil, []map[string]any{{
		"id": "turn-exact", "status": "inProgress", "items": []any{},
	}})
	agent := &Agent{
		backend: "app_server", appServerTransport: appServerTransportDaemon,
		appServerSocket: daemon.socketPath, workDir: workDir,
	}
	t.Cleanup(func() { _ = agent.Stop() })

	if err := agent.SteerConversationTurn(context.Background(), "thread-exact", "turn-exact", "continue here", "feishu-message", nil, nil); err != nil {
		t.Fatalf("SteerConversationTurn() error = %v", err)
	}
	select {
	case request := <-daemon.steers:
		if request.ThreadID != "thread-exact" || request.ExpectedTurnID != "turn-exact" || request.ClientUserMessageID != "feishu-message" {
			t.Fatalf("turn/steer request = %#v", request)
		}
		if len(request.Input) != 1 || stringMapValue(request.Input[0], "text") != "continue here" {
			t.Fatalf("turn/steer input = %#v", request.Input)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for turn/steer")
	}
	daemon.mu.Lock()
	clients := append([]*fakeSharedAppServerClient(nil), daemon.clients...)
	daemon.mu.Unlock()
	for _, client := range clients {
		if client.resumeCalls != 0 {
			t.Fatalf("steer control connection issued thread/resume")
		}
	}
}

func TestAgentGetConversationWindow_PagesUntilWatermark(t *testing.T) {
	daemon := newFakeSharedAppServerDaemon(t)
	workDir := t.TempDir()
	turns := make([]map[string]any, 0, 205)
	for index := 0; index < 205; index++ {
		turns = append(turns, map[string]any{
			"id": fmt.Sprintf("turn-%03d", index), "status": "completed", "items": []any{},
		})
	}
	daemon.setConversation(workDir, "thread-paged", "idle", nil, turns)
	agent := &Agent{
		backend: "app_server", appServerTransport: appServerTransportDaemon,
		appServerSocket: daemon.socketPath, workDir: workDir,
	}
	t.Cleanup(func() { _ = agent.Stop() })

	snapshot, covered, err := agent.GetConversationWindow(context.Background(), "thread-paged", "turn-150", 200)
	if err != nil {
		t.Fatalf("GetConversationWindow() error = %v", err)
	}
	if !covered {
		t.Fatal("watermark should be covered across two pages")
	}
	if len(snapshot.Turns) != 151 || snapshot.Turns[0].ID != "turn-150" || snapshot.Turns[len(snapshot.Turns)-1].ID != "turn-000" {
		t.Fatalf("paged turn window = len %d, first %q, last %q", len(snapshot.Turns), snapshot.Turns[0].ID, snapshot.Turns[len(snapshot.Turns)-1].ID)
	}

	_, covered, err = agent.GetConversationWindow(context.Background(), "thread-paged", "turn-missing", 120)
	if err != nil {
		t.Fatalf("missing watermark read error = %v", err)
	}
	if covered {
		t.Fatal("missing watermark unexpectedly reported covered")
	}
}

func TestReadAppServerConversation_RejectsOtherWorkspace(t *testing.T) {
	daemon := newFakeSharedAppServerDaemon(t)
	daemon.setConversation(t.TempDir(), "thread-other", "idle", nil, nil)
	client, err := newAppServerConversationClient(daemon.socketPath, t.TempDir())
	if err != nil {
		t.Fatalf("newAppServerConversationClient() error = %v", err)
	}
	defer client.Close()

	_, err = readAppServerConversation(client, "thread-other", t.TempDir(), 1)
	if err == nil || !strings.Contains(err.Error(), "refusing thread") {
		t.Fatalf("workspace mismatch error = %v", err)
	}
}
