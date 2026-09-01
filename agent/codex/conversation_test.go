package codex

import (
	"context"
	"encoding/json"
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

func TestMapAppServerConversationTurn_ProposedPlanIsVisibleFinalResponse(t *testing.T) {
	turn := appServerConversationTurn{
		ID:     "turn-plan",
		Status: "completed",
		Items: []map[string]any{
			{"type": "userMessage", "id": "prompt", "content": []any{map[string]any{"type": "text", "text": "make a plan"}}},
			{"type": "plan", "id": "plan", "text": "# Proposed plan\n\n- Implement it"},
		},
	}

	mapped := mapAppServerConversationTurn(turn)
	if len(mapped.Messages) != 2 {
		t.Fatalf("messages = %#v, want user and proposed plan", mapped.Messages)
	}
	plan := mapped.Messages[1]
	if plan.Role != "assistant" || plan.Phase != "proposed_plan" || !strings.Contains(plan.Content, "Implement it") {
		t.Fatalf("proposed plan message = %#v", plan)
	}
	if len(mapped.Activities) != 0 {
		t.Fatalf("plan was rendered as an activity: %#v", mapped.Activities)
	}
	if len(mapped.PresentationEvents) != 1 || mapped.PresentationEvents[0].Type != core.EventText || mapped.PresentationEvents[0].Content != plan.Content {
		t.Fatalf("plan presentation = %#v, want one final text event", mapped.PresentationEvents)
	}
}

func TestMapAppServerConversationTurn_HidesRequestUserInputTransportJSON(t *testing.T) {
	turn := appServerConversationTurn{
		ID:     "turn-waiting",
		Status: "inProgress",
		Items: []map[string]any{
			{"type": "agentMessage", "id": "commentary-1", "text": "I need one choice."},
			{
				"type": "dynamicToolCall", "id": "question-1", "tool": "request_user_input", "status": "completed",
				"arguments": map[string]any{"questions": []any{map[string]any{
					"id": "database", "question": "Which database?",
					"options": []any{map[string]any{"label": "Postgres"}},
				}}},
				"contentItems": []any{map[string]any{"type": "inputText", "text": `{"answers":{"database":["Postgres"]}}`}},
			},
		},
	}

	mapped := mapAppServerConversationTurn(turn)
	if len(mapped.Activities) != 0 {
		t.Fatalf("request_user_input activities = %#v, want none", mapped.Activities)
	}
	if len(mapped.PresentationEvents) != 1 || mapped.PresentationEvents[0].Type != core.EventThinking || mapped.PresentationEvents[0].Content != "I need one choice." {
		t.Fatalf("request_user_input presentation = %#v, want preceding commentary only", mapped.PresentationEvents)
	}
	serialized, err := json.Marshal(mapped)
	if err != nil {
		t.Fatalf("marshal mapped turn: %v", err)
	}
	for _, forbidden := range []string{"request_user_input", "Which database?", `\"answers\"`} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("mapped turn leaked request transport %q: %s", forbidden, serialized)
		}
	}
}

func TestMapAppServerConversationTurn_RecoversPendingRequestUserInput(t *testing.T) {
	tests := []struct {
		name      string
		arguments any
	}{
		{
			name: "object arguments",
			arguments: map[string]any{"questions": []any{
				map[string]any{
					"id": "scope", "header": "Scope", "question": "Which scope?", "isOther": true,
					"options": []any{
						map[string]any{"label": "Repository", "description": "Inspect all files"},
						map[string]any{"label": "Diff", "description": "Inspect changed files"},
					},
				},
				map[string]any{
					"id": "token", "header": "Secret", "question": "Enter the token", "isSecret": true,
				},
			}},
		},
		{
			name:      "JSON string arguments",
			arguments: `{"questions":[{"id":"scope","header":"Scope","question":"Which scope?","isOther":true,"options":[{"label":"Repository","description":"Inspect all files"},{"label":"Diff","description":"Inspect changed files"}]},{"id":"token","header":"Secret","question":"Enter the token","isSecret":true}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapped := mapAppServerConversationTurn(appServerConversationTurn{
				ID: "turn-waiting", Status: "inProgress",
				Items: []map[string]any{
					{"type": "agentMessage", "id": "commentary", "text": "I need two choices."},
					{
						"type": "dynamicToolCall", "id": "question-1", "tool": "request_user_input",
						"status": "inProgress", "arguments": tt.arguments,
					},
				},
			})

			if mapped.PendingInput == nil || mapped.PendingInput.ItemID != "question-1" {
				t.Fatalf("pending input = %#v", mapped.PendingInput)
			}
			questions := mapped.PendingInput.Questions
			if len(questions) != 2 || questions[0].Question != "Which scope?" || questions[0].Header != "Scope" {
				t.Fatalf("questions = %#v", questions)
			}
			if len(questions[0].Options) != 2 || questions[0].Options[0].Label != "Repository" ||
				questions[0].Options[1].Description != "Inspect changed files" || !questions[1].Secret {
				t.Fatalf("question details = %#v", questions)
			}
			if len(mapped.Activities) != 0 {
				t.Fatalf("request_user_input activities = %#v, want none", mapped.Activities)
			}
			if len(mapped.PresentationEvents) != 1 || mapped.PresentationEvents[0].Content != "I need two choices." {
				t.Fatalf("presentation = %#v", mapped.PresentationEvents)
			}
		})
	}
}

func TestMapAppServerConversationTurn_DoesNotRecoverCompletedRequestUserInput(t *testing.T) {
	mapped := mapAppServerConversationTurn(appServerConversationTurn{
		ID: "turn-running", Status: "inProgress",
		Items: []map[string]any{{
			"type": "dynamicToolCall", "id": "question-1", "tool": "request_user_input", "status": "completed",
			"arguments": map[string]any{"questions": []any{map[string]any{"id": "scope", "question": "Which scope?"}}},
		}},
	})
	if mapped.PendingInput != nil {
		t.Fatalf("completed pending input = %#v, want nil", mapped.PendingInput)
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
				map[string]any{
					"type": "dynamicToolCall", "id": "question-new", "tool": "request_user_input", "status": "inProgress",
					"arguments": `{"questions":[{"id":"scope","header":"Scope","question":"Which scope?","options":[{"label":"Repository","description":"Inspect all files"}]}]}`,
				},
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
	if got := snapshot.Turns[0].Activities; len(got) != 0 {
		t.Fatalf("plan activities = %#v, want none", got)
	}
	if got := snapshot.Turns[0].Messages; len(got) != 4 || got[3].Phase != "proposed_plan" || got[3].Content != "private plan detail" {
		t.Fatalf("proposed plan message = %#v", got)
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
	if latest.PendingInput == nil || latest.PendingInput.ItemID != "question-new" ||
		len(latest.PendingInput.Questions) != 1 || latest.PendingInput.Questions[0].Question != "Which scope?" {
		t.Fatalf("latest pending input = %#v", latest.PendingInput)
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
		latest.Messages[0].Content,
		latest.Messages[1].Content,
		latest.Activities[0].Kind,
		latest.Activities[0].Status,
	}, "\n")
	for _, forbidden := range []string{"cat /secret/path", "private reasoning", "secret preamble"} {
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

	daemon.sendRequestToClient(t, clients[0], 72, "item/tool/requestUserInput", map[string]any{
		"threadId": threadID, "turnId": "turn-live", "itemId": "question-passive",
		"questions": []any{map[string]any{
			"id": "database", "question": "Should passive watch answer?",
			"options": []any{map[string]any{"label": "No"}},
		}},
	})
	daemon.assertNoResponse(t, 100*time.Millisecond)
}

func TestAgentOpenConversationObserver_ResumesAndAnswersSharedUserInput(t *testing.T) {
	daemon := newFakeSharedAppServerDaemon(t)
	workDir := t.TempDir()
	threadID := "thread-interactive-observer"
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

	observer, err := agent.OpenConversationObserver(ctx, threadID)
	if err != nil {
		t.Fatalf("OpenConversationObserver() error = %v", err)
	}
	daemon.waitForClients(t, 1)
	clients := daemon.snapshotClients(t)
	if got := clients[0].resumeCalls; got != 1 {
		t.Fatalf("interactive observer thread/resume calls = %d, want 1", got)
	}
	if !clients[0].resumeExcludesTurns {
		t.Fatal("interactive observer thread/resume excludeTurns = false, want true")
	}

	daemon.sendRequestToThread(t, threadID, 73, "item/tool/requestUserInput", map[string]any{
		"threadId": threadID,
		"turnId":   "turn-live",
		"itemId":   "question-1",
		"questions": []any{map[string]any{
			"id": "database", "header": "Database", "question": "Which database?",
			"options": []any{map[string]any{"label": "Postgres"}, map[string]any{"label": "SQLite"}},
		}},
	})

	var request core.Event
	select {
	case request = <-observer.Events():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shared requestUserInput")
	}
	if request.Type != core.EventPermissionRequest || request.RequestID != "73" ||
		request.ThreadID != threadID || request.TurnID != "turn-live" || request.ItemID != "question-1" {
		t.Fatalf("observer request event = %#v", request)
	}
	if len(request.Questions) != 1 || request.Questions[0].Question != "Which database?" {
		t.Fatalf("observer questions = %#v", request.Questions)
	}
	if err := observer.RespondPermission(request.RequestID, core.PermissionResult{
		Behavior: "allow",
		UpdatedInput: map[string]any{"answers": map[string]any{
			"Which database?": "SQLite",
		}},
	}); err != nil {
		t.Fatalf("RespondPermission() error = %v", err)
	}
	response := daemon.waitForResponse(t, 73)
	var result struct {
		Answers map[string]struct {
			Answers []string `json:"answers"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(response.payload["result"], &result); err != nil {
		t.Fatalf("decode observer response: %v", err)
	}
	if got := result.Answers["database"].Answers; len(got) != 1 || got[0] != "SQLite" {
		t.Fatalf("observer response answers = %#v, want [SQLite]", got)
	}

	daemon.broadcast(t, "serverRequest/resolved", map[string]any{
		"threadId": threadID, "requestId": 73,
	})
	select {
	case event := <-observer.Events():
		if event.Type != core.EventPermissionResolved || event.RequestID != "73" || event.ThreadID != threadID {
			t.Fatalf("observer resolution event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for shared request resolution")
	}
	daemon.broadcast(t, "thread/status/changed", map[string]any{
		"threadId": threadID, "status": map[string]any{"type": "idle"},
	})
	select {
	case event := <-observer.Events():
		if event.Type != core.EventResult || !event.Done || event.ThreadID != threadID || event.TurnID != "turn-live" {
			t.Fatalf("observer terminal event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for observer terminal event")
	}

	// Rejoining a thread only permits requestUserInput. A passive observer must
	// never answer a command approval or otherwise become the turn writer.
	daemon.sendRequestToThread(t, threadID, 74, "item/commandExecution/requestApproval", map[string]any{
		"threadId": threadID, "turnId": "turn-live", "itemId": "command-1", "command": "pwd",
	})
	daemon.assertNoResponse(t, 100*time.Millisecond)
}

func TestAgentOpenConversationObserver_ProgressFloodPreservesBlockingQuestionLifecycle(t *testing.T) {
	daemon := newFakeSharedAppServerDaemon(t)
	workDir := t.TempDir()
	threadID := "thread-flooded-observer"
	daemon.setConversation(workDir, threadID, "active", []string{"waitingOnUserInput"}, []map[string]any{{
		"id": "turn-live", "status": "inProgress", "items": []any{},
	}})
	agent := &Agent{
		backend: "app_server", appServerTransport: appServerTransportDaemon,
		appServerSocket: daemon.socketPath, workDir: workDir,
	}
	t.Cleanup(func() { _ = agent.Stop() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	observer, err := agent.OpenConversationObserver(ctx, threadID)
	if err != nil {
		t.Fatalf("OpenConversationObserver() error = %v", err)
	}
	daemon.waitForClients(t, 1)

	// Do not consume observer.Events while flooding it. Before progress
	// coalescing, the two 256-entry FIFOs filled with delta wakeups and the
	// following requestUserInput event was silently dropped.
	for index := 0; index < appServerConversationEventBuffer*6; index++ {
		daemon.broadcast(t, "item/agentMessage/delta", map[string]any{
			"threadId": threadID, "turnId": "turn-live", "itemId": fmt.Sprintf("delta-%d", index), "delta": "x",
		})
	}
	time.Sleep(100 * time.Millisecond)
	daemon.sendRequestToThread(t, threadID, 75, "item/tool/requestUserInput", map[string]any{
		"threadId": threadID, "turnId": "turn-live", "itemId": "question-flood",
		"questions": []any{map[string]any{
			"id": "database", "question": "Which database?",
			"options": []any{map[string]any{"label": "Postgres"}, map[string]any{"label": "SQLite"}},
		}},
	})
	daemon.broadcast(t, "serverRequest/resolved", map[string]any{
		"threadId": threadID, "requestId": 75,
	})
	daemon.broadcast(t, "thread/status/changed", map[string]any{
		"threadId": threadID, "status": map[string]any{"type": "idle"},
	})

	want := []core.EventType{core.EventPermissionRequest, core.EventPermissionResolved, core.EventResult}
	got := make([]core.EventType, 0, len(want))
	deadline := time.After(3 * time.Second)
	for len(got) < len(want) {
		select {
		case event, ok := <-observer.Events():
			if !ok {
				t.Fatalf("observer closed after events %v", got)
			}
			if appServerObserverEventRequiresDelivery(event) {
				got = append(got, event.Type)
			}
		case <-deadline:
			t.Fatalf("blocking question lifecycle events = %v, want %v", got, want)
		}
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("blocking question lifecycle events = %v, want %v", got, want)
		}
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
	daemon.mu.Lock()
	limits := append([]int(nil), daemon.turnListLimits...)
	daemon.mu.Unlock()
	if len(limits) < 2 {
		t.Fatalf("thread/turns/list calls = %d, want paginated reads", len(limits))
	}
	for _, limit := range limits {
		if limit > 10 {
			t.Fatalf("thread/turns/list limit = %d, want at most 10", limit)
		}
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
