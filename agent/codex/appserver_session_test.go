package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

func TestAppServerSession_ConnectsWithWebSocketFramesOverUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are not available on Windows")
	}
	socketFile, err := os.CreateTemp("", "cc-codex-*.sock")
	if err != nil {
		t.Fatalf("reserve Unix socket path: %v", err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatalf("close temporary socket file: %v", err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("remove temporary socket file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on Unix socket: %v", err)
	}
	serverResult := make(chan error, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			serverResult <- fmt.Errorf("upgrade websocket: %w", err)
			return
		}
		defer conn.Close()

		var initialize map[string]any
		if err := conn.ReadJSON(&initialize); err != nil {
			serverResult <- fmt.Errorf("read initialize frame: %w", err)
			return
		}
		if initialize["method"] != "initialize" {
			serverResult <- fmt.Errorf("first method = %#v, want initialize", initialize["method"])
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"id":     initialize["id"],
			"result": map[string]any{"userAgent": "test-app-server"},
		}); err != nil {
			serverResult <- fmt.Errorf("write initialize response: %w", err)
			return
		}

		var initialized map[string]any
		if err := conn.ReadJSON(&initialized); err != nil {
			serverResult <- fmt.Errorf("read initialized frame: %w", err)
			return
		}
		if initialized["method"] != "initialized" {
			serverResult <- fmt.Errorf("second method = %#v, want initialized", initialized["method"])
			return
		}
		serverResult <- nil
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	s := &appServerSession{
		transport:        appServerTransportDaemon,
		socketPath:       socketPath,
		ctx:              ctx,
		cancel:           cancel,
		events:           make(chan core.Event, 4),
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}
	s.alive.Store(true)
	if err := s.connect(); err != nil {
		t.Fatalf("connect() error: %v", err)
	}
	defer s.Close()
	if err := s.initialize(); err != nil {
		t.Fatalf("initialize() error: %v", err)
	}

	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive WebSocket initialize handshake")
	}
}

func TestAppServerSession_LiveDaemonReadOnly(t *testing.T) {
	socketPath := strings.TrimSpace(os.Getenv("CC_CONNECT_CODEX_APP_SERVER_SOCKET"))
	if socketPath == "" {
		t.Skip("set CC_CONNECT_CODEX_APP_SERVER_SOCKET to run the live daemon test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	s := &appServerSession{
		transport:        appServerTransportDaemon,
		socketPath:       socketPath,
		ctx:              ctx,
		cancel:           cancel,
		events:           make(chan core.Event, 4),
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}
	s.alive.Store(true)
	if err := s.connect(); err != nil {
		t.Fatalf("connect() error: %v", err)
	}
	defer s.Close()
	if err := s.initialize(); err != nil {
		t.Fatalf("initialize() error: %v", err)
	}

	var response struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := s.requestWithTimeout("thread/list", map[string]any{
		"limit":          1,
		"useStateDbOnly": true,
	}, &response, 5*time.Second); err != nil {
		t.Fatalf("thread/list error: %v", err)
	}
	if len(response.Data) > 1 {
		t.Fatalf("thread/list returned %d entries, want at most 1", len(response.Data))
	}
}

func TestResolveAppServerSocket_UsesExplicitSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := resolveAppServerSocket(" ~/.codex/custom.sock ", "/ignored", []string{"CODEX_HOME=/also-ignored"})
	if err != nil {
		t.Fatalf("resolveAppServerSocket() error: %v", err)
	}
	want := filepath.Join(home, ".codex", "custom.sock")
	if got != want {
		t.Fatalf("resolveAppServerSocket() = %q, want %q", got, want)
	}
}

func TestResolveAppServerSocket_DefaultsToConfiguredCodexHome(t *testing.T) {
	codexHome := t.TempDir()
	got, err := resolveAppServerSocket("", codexHome, []string{"CODEX_HOME=/ignored"})
	if err != nil {
		t.Fatalf("resolveAppServerSocket() error: %v", err)
	}
	want := filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
	if got != want {
		t.Fatalf("resolveAppServerSocket() = %q, want %q", got, want)
	}
}

func TestAppServerSession_DaemonThreadStartParamsIncludeAbsoluteWorkDir(t *testing.T) {
	s := &appServerSession{transport: appServerTransportDaemon, workDir: ".", model: "gpt-5.4", mode: "suggest"}
	wantWorkDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("filepath.Abs(): %v", err)
	}

	params := s.threadStartRequestParams()
	if got := params["cwd"]; got != wantWorkDir {
		t.Fatalf("thread start cwd = %#v, want %q", got, wantWorkDir)
	}
	if got := s.threadRequestParams()["cwd"]; got != nil {
		t.Fatalf("resume params cwd = %#v, want nil so the canonical thread cwd is preserved", got)
	}
}

func TestAppServerSession_ProcessThreadStartParamsPreserveLegacyShape(t *testing.T) {
	s := &appServerSession{transport: appServerTransportProcess, workDir: "."}
	if got := s.threadStartRequestParams()["cwd"]; got != nil {
		t.Fatalf("process thread start cwd = %#v, want nil", got)
	}
}

func TestAppServerSession_ApplyThreadRuntimeState(t *testing.T) {
	s := &appServerSession{}
	effort := "xhigh"

	s.applyThreadRuntimeState("/tmp/project", "gpt-5.4", &effort)

	if got := s.GetWorkDir(); got != "/tmp/project" {
		t.Fatalf("GetWorkDir() = %q, want /tmp/project", got)
	}
	if got := s.GetModel(); got != "gpt-5.4" {
		t.Fatalf("GetModel() = %q, want gpt-5.4", got)
	}
	if got := s.GetReasoningEffort(); got != "xhigh" {
		t.Fatalf("GetReasoningEffort() = %q, want xhigh", got)
	}
}

func TestAppServerSession_HandleRateLimitsUpdatedCachesUsage(t *testing.T) {
	s := &appServerSession{}
	raw, err := json.Marshal(appServerRateLimitsResponse{
		RateLimits: appServerRateLimitSnapshot{
			LimitID:   "codex",
			PlanType:  "pro",
			Primary:   &appServerRateLimitWindow{UsedPercent: 25, WindowDurationMins: 15, ResetsAt: 1730947200},
			Secondary: &appServerRateLimitWindow{UsedPercent: 42, WindowDurationMins: 60, ResetsAt: 1730950800},
			Credits:   &appServerCreditsSnapshot{HasCredits: true, Unlimited: false},
		},
	})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}

	s.handleNotification("account/rateLimits/updated", raw)

	report, err := s.GetUsage(context.Background())
	if err != nil {
		t.Fatalf("GetUsage() returned error: %v", err)
	}
	if report.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", report.Provider)
	}
	if report.Plan != "pro" {
		t.Fatalf("plan = %q, want pro", report.Plan)
	}
	if len(report.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(report.Buckets))
	}
	if got := report.Buckets[0].Name; got != "codex" {
		t.Fatalf("bucket name = %q, want codex", got)
	}
	if got := report.Buckets[0].Windows[0].WindowSeconds; got != 15*60 {
		t.Fatalf("primary window seconds = %d, want %d", got, 15*60)
	}
	if got := report.Buckets[0].Windows[1].UsedPercent; got != 42 {
		t.Fatalf("secondary used percent = %d, want 42", got)
	}
	if report.Credits == nil || !report.Credits.HasCredits {
		t.Fatalf("credits = %#v, want has credits", report.Credits)
	}
}

func TestAppServerSession_HandleThreadTokenUsageUpdatedCachesContextUsage(t *testing.T) {
	s := &appServerSession{}
	s.threadID.Store("thread-1")
	raw, err := json.Marshal(appServerThreadTokenUsageNotification{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		TokenUsage: struct {
			Total              codexTokenUsage `json:"total"`
			Last               codexTokenUsage `json:"last"`
			ModelContextWindow int             `json:"modelContextWindow"`
		}{
			Total: codexTokenUsage{
				TotalTokens:           52011395,
				InputTokens:           51847383,
				CachedInputTokens:     48187904,
				OutputTokens:          164012,
				ReasoningOutputTokens: 78910,
			},
			Last: codexTokenUsage{
				TotalTokens:           41061,
				InputTokens:           40849,
				CachedInputTokens:     36864,
				OutputTokens:          212,
				ReasoningOutputTokens: 32,
			},
			ModelContextWindow: 258400,
		},
	})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}

	s.handleNotification("thread/tokenUsage/updated", raw)

	usage := s.GetContextUsage()
	if usage == nil {
		t.Fatal("GetContextUsage() = nil, want cached context usage")
	}
	if usage.UsedTokens != 41061 {
		t.Fatalf("used tokens = %d, want 41061", usage.UsedTokens)
	}
	if usage.BaselineTokens != codexContextBaselineTokens {
		t.Fatalf("baseline tokens = %d, want %d", usage.BaselineTokens, codexContextBaselineTokens)
	}
	if usage.TotalTokens != 41061 {
		t.Fatalf("total tokens = %d, want 41061", usage.TotalTokens)
	}
	if usage.ContextWindow != 258400 {
		t.Fatalf("context window = %d, want 258400", usage.ContextWindow)
	}
	if usage.CachedInputTokens != 36864 {
		t.Fatalf("cached input tokens = %d, want 36864", usage.CachedInputTokens)
	}
	if usage.InputTokens != 40849 {
		t.Fatalf("input tokens = %d, want 40849", usage.InputTokens)
	}
}

func TestAppServerSession_FiltersEveryThreadScopedNotification(t *testing.T) {
	tests := []struct {
		name   string
		method string
		params any
	}{
		{
			name:   "turn started",
			method: "turn/started",
			params: map[string]any{"threadId": "thread-B", "turn": map[string]any{"id": "turn-B", "status": "inProgress"}},
		},
		{
			name:   "item started",
			method: "item/started",
			params: map[string]any{"threadId": "thread-B", "turnId": "turn-B", "item": map[string]any{"type": "commandExecution", "command": "echo leaked"}},
		},
		{
			name:   "item completed",
			method: "item/completed",
			params: map[string]any{"threadId": "thread-B", "turnId": "turn-B", "item": map[string]any{"type": "agentMessage", "text": "leaked text"}},
		},
		{
			name:   "turn completed",
			method: "turn/completed",
			params: map[string]any{"threadId": "thread-B", "turn": map[string]any{"id": "turn-B", "status": "completed"}},
		},
		{
			name:   "thread idle",
			method: "thread/status/changed",
			params: map[string]any{"threadId": "thread-B", "status": map[string]any{"type": "idle"}},
		},
		{
			name:   "token usage",
			method: "thread/tokenUsage/updated",
			params: map[string]any{
				"threadId": "thread-B",
				"turnId":   "turn-B",
				"tokenUsage": map[string]any{
					"last":               map[string]any{"totalTokens": 999},
					"modelContextWindow": 1000,
				},
			},
		},
		{
			name:   "turn error",
			method: "error",
			params: map[string]any{"threadId": "thread-B", "turnId": "turn-B", "error": map[string]any{"message": "leaked error"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newIsolatedAppServerTestSession("thread-A", "turn-A", 16)
			s.pendingMsgs = []string{"keep me"}
			s.storeContextUsage(&core.ContextUsage{UsedTokens: 7, TotalTokens: 7, ContextWindow: 100})

			s.handleNotification(tt.method, mustMarshalAppServerTest(t, tt.params))

			assertAppServerStateUnchanged(t, s, "turn-A", []string{"keep me"}, 7)
			assertNoAppServerEvent(t, s.events)
		})
	}
}

func TestAppServerSession_ThreadScopedNotificationsFailClosedWithoutValidThreadID(t *testing.T) {
	methods := []string{
		"turn/started",
		"item/started",
		"item/completed",
		"turn/completed",
		"thread/status/changed",
		"thread/tokenUsage/updated",
		"error",
	}
	payloads := []json.RawMessage{
		mustMarshalAppServerTest(t, map[string]any{}),
		mustMarshalAppServerTest(t, map[string]any{"threadId": ""}),
		json.RawMessage(`{"threadId":42}`),
		json.RawMessage(`{"threadId":`),
	}

	for _, method := range methods {
		for i, payload := range payloads {
			t.Run(fmt.Sprintf("%s/payload-%d", method, i), func(t *testing.T) {
				s := newIsolatedAppServerTestSession("thread-A", "turn-A", 4)
				s.pendingMsgs = []string{"keep me"}
				s.storeContextUsage(&core.ContextUsage{UsedTokens: 7, TotalTokens: 7, ContextWindow: 100})

				s.handleNotification(method, payload)

				assertAppServerStateUnchanged(t, s, "turn-A", []string{"keep me"}, 7)
				assertNoAppServerEvent(t, s.events)
			})
		}
	}
}

func TestAppServerSession_ForegroundDeltaFloodPreservesLogicalProgressEvents(t *testing.T) {
	s := newIsolatedAppServerTestSession("thread-long", "turn-long", 1)
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < 1000; i++ {
		s.dispatchNotification("item/commandExecution/outputDelta", mustMarshalAppServerTest(t, map[string]any{
			"threadId": "thread-long", "turnId": "turn-long", "itemId": "command-1", "delta": "x",
		}))
	}
	if got := len(s.events); got != 0 {
		t.Fatalf("foreground output deltas queued %d observer-only events", got)
	}

	s.dispatchNotification("item/started", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-long", "turnId": "turn-long",
		"item": map[string]any{"type": "commandExecution", "id": "command-1", "command": "pwd"},
	}))
	completed := make(chan struct{})
	go func() {
		s.dispatchNotification("item/completed", mustMarshalAppServerTest(t, map[string]any{
			"threadId": "thread-long", "turnId": "turn-long",
			"item": map[string]any{
				"type": "commandExecution", "id": "command-1", "command": "pwd",
				"status": "completed", "aggregatedOutput": "/workspace", "exitCode": 0,
			},
		}))
		close(completed)
	}()

	select {
	case <-completed:
		t.Fatal("tool result was dropped instead of applying backpressure")
	case <-time.After(20 * time.Millisecond):
	}
	first := waitForAppServerEvent(t, s.events)
	if first.Type != core.EventToolUse || first.ToolInput != "pwd" {
		t.Fatalf("first logical progress event = %#v, want tool use", first)
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("tool result producer remained blocked after the consumer drained")
	}
	second := waitForAppServerEvent(t, s.events)
	if second.Type != core.EventToolResult || second.ToolResult != "/workspace" {
		t.Fatalf("second logical progress event = %#v, want tool result", second)
	}
}

func TestAppServerSession_InterleavedNotificationsStayWithBoundThread(t *testing.T) {
	sessionA := newIsolatedAppServerTestSession("thread-A", "", 512)
	sessionB := newIsolatedAppServerTestSession("thread-B", "", 512)
	sessions := []*appServerSession{sessionA, sessionB}

	broadcast := func(method string, params any) {
		raw := mustMarshalAppServerTest(t, params)
		for _, session := range sessions {
			session.handleNotification(method, raw)
		}
	}

	for i := 0; i < 100; i++ {
		for threadIndex, threadID := range []string{"thread-A", "thread-B"} {
			turnID := fmt.Sprintf("%s-turn-%d", threadID, i)
			usedTokens := i*10 + threadIndex + 1
			broadcast("turn/started", map[string]any{
				"threadId": threadID,
				"turn":     map[string]any{"id": turnID, "status": "inProgress"},
			})
			broadcast("item/started", map[string]any{
				"threadId": threadID,
				"turnId":   turnID,
				"item":     map[string]any{"type": "commandExecution", "command": "tool-" + threadID},
			})
			broadcast("item/completed", map[string]any{
				"threadId": threadID,
				"turnId":   turnID,
				"item": map[string]any{
					"type": "commandExecution", "command": "tool-" + threadID,
					"status": "completed", "aggregatedOutput": "output-" + threadID, "exitCode": 0,
				},
			})
			broadcast("item/completed", map[string]any{
				"threadId": threadID,
				"turnId":   turnID,
				"item":     map[string]any{"type": "agentMessage", "text": "reply-" + threadID},
			})
			broadcast("thread/tokenUsage/updated", map[string]any{
				"threadId": threadID,
				"turnId":   turnID,
				"tokenUsage": map[string]any{
					"last":               map[string]any{"totalTokens": usedTokens},
					"modelContextWindow": 1000,
				},
			})
			if i%2 == 0 {
				broadcast("turn/completed", map[string]any{
					"threadId": threadID,
					"turn":     map[string]any{"id": turnID, "status": "completed"},
				})
			} else {
				broadcast("thread/status/changed", map[string]any{
					"threadId": threadID,
					"status":   map[string]any{"type": "idle"},
				})
			}
		}
	}

	assertThreadEvents := func(t *testing.T, session *appServerSession, own, other string) {
		t.Helper()
		// Each iteration emits turn-started, tool-use, tool-result, agent text,
		// and one terminal result for the session's bound thread.
		for i := 0; i < 500; i++ {
			select {
			case event := <-session.events:
				joined := strings.Join([]string{event.Content, event.ToolInput, event.ToolResult, event.SessionID}, "\n")
				if strings.Contains(joined, other) {
					t.Fatalf("event %d leaked %s into %s: %#v", i, other, own, event)
				}
				if event.Type == core.EventResult && event.SessionID != own {
					t.Fatalf("result session id = %q, want %q", event.SessionID, own)
				}
			case <-time.After(time.Second):
				t.Fatalf("received fewer than 500 events for %s", own)
			}
		}
		assertNoAppServerEvent(t, session.events)
	}

	assertThreadEvents(t, sessionA, "thread-A", "thread-B")
	assertThreadEvents(t, sessionB, "thread-B", "thread-A")
	if usage := sessionA.cachedContextUsage(); usage == nil || usage.UsedTokens != 991 {
		t.Fatalf("thread-A context usage = %#v, want 991 tokens", usage)
	}
	if usage := sessionB.cachedContextUsage(); usage == nil || usage.UsedTokens != 992 {
		t.Fatalf("thread-B context usage = %#v, want 992 tokens", usage)
	}
}

func TestAppServerSession_NewThreadBuffersNotificationsUntilBound(t *testing.T) {
	s := newIsolatedAppServerTestSession("", "", 8)
	s.beginNotificationBuffer()
	s.handleNotification("turn/started", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-B", "turn": map[string]any{"id": "turn-B", "status": "inProgress"},
	}))
	s.handleNotification("turn/started", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-A", "turn": map[string]any{"id": "turn-A", "status": "inProgress"},
	}))
	s.handleNotification("item/completed", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-A", "turnId": "turn-A", "item": map[string]any{"type": "agentMessage", "text": "reply-A"},
	}))

	assertAppServerStateUnchanged(t, s, "", nil, 0)
	if got := len(s.bufferedNotifications); got != 3 {
		t.Fatalf("buffered notifications = %d, want 3 before binding", got)
	}
	if err := s.bindThread("thread-A"); err != nil {
		t.Fatalf("bindThread() error: %v", err)
	}

	s.stateMu.Lock()
	turnID := s.currentTurn
	pending := append([]string(nil), s.pendingMsgs...)
	s.stateMu.Unlock()
	if turnID != "turn-A" || len(pending) != 1 || pending[0] != "reply-A" {
		t.Fatalf("replayed state = turn %q pending %#v, want only thread-A", turnID, pending)
	}
	if got := len(s.bufferedNotifications); got != 0 {
		t.Fatalf("buffered notifications = %d, want drained", got)
	}
}

func TestAppServerSession_CompletedPlanEmitsFinalText(t *testing.T) {
	s := newIsolatedAppServerTestSession("thread-plan", "turn-plan", 4)
	t.Cleanup(func() { _ = s.Close() })

	s.handleItemCompleted(itemNotification{
		ThreadID: "thread-plan",
		TurnID:   "turn-plan",
		Item:     map[string]any{"type": "plan", "id": "plan-item", "text": "# Proposed plan\n\n- Execute it"},
	})
	s.completeTurn("thread-plan", "turn-plan", "completed")

	textEvent := waitForAppServerEvent(t, s.events)
	if textEvent.Type != core.EventText || textEvent.Content != "# Proposed plan\n\n- Execute it" || textEvent.TurnID != "turn-plan" {
		t.Fatalf("plan text event = %#v", textEvent)
	}
	resultEvent := waitForAppServerEvent(t, s.events)
	if resultEvent.Type != core.EventResult || !resultEvent.Done || resultEvent.TurnID != "turn-plan" {
		t.Fatalf("plan result event = %#v", resultEvent)
	}
}

func TestAppServerSession_LiveRequestUserInputHidesTransportJSON(t *testing.T) {
	s := newIsolatedAppServerTestSession("thread-question", "turn-question", 4)
	t.Cleanup(func() { _ = s.Close() })
	s.pendingMsgs = []string{"I need one choice before continuing."}

	item := map[string]any{
		"type": "dynamicToolCall",
		"id":   "question-item",
		"tool": "request_user_input",
		"arguments": map[string]any{
			"questions": []any{map[string]any{
				"id": "scope", "question": "Which scope?",
			}},
		},
		"status":       "completed",
		"contentItems": []any{map[string]any{"type": "inputText", "text": `{"scope":"all"}`}},
	}
	s.handleItemStarted(itemNotification{ThreadID: "thread-question", TurnID: "turn-question", Item: item})

	event := waitForAppServerEvent(t, s.events)
	if event.Type != core.EventThinking || event.Content != "I need one choice before continuing." {
		t.Fatalf("pre-question event = %#v, want commentary without request JSON", event)
	}
	s.handleItemCompleted(itemNotification{ThreadID: "thread-question", TurnID: "turn-question", Item: item})
	assertNoAppServerEvent(t, s.events)
}

func TestAppServerSession_ThreadStartResponseMayFollowNotifications(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		transport:        appServerTransportProcess,
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		cancel:           cancel,
		stdin:            stdin,
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}

	done := make(chan error, 1)
	go func() { done <- s.ensureThread("") }()
	request := waitForAppServerClientRequest(t, stdin, "thread/start")

	s.handleNotification("turn/started", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-B", "turn": map[string]any{"id": "turn-B"},
	}))
	s.handleNotification("turn/started", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-A", "turn": map[string]any{"id": "turn-A"},
	}))
	s.handleNotification("item/completed", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-A", "turnId": "turn-A", "item": map[string]any{"type": "agentMessage", "text": "only A"},
	}))
	s.handleResponse(rpcResponseEnvelope{
		ID:     request.ID,
		Result: mustMarshalAppServerTest(t, map[string]any{"thread": map[string]any{"id": "thread-A"}}),
	})

	if err := <-done; err != nil {
		t.Fatalf("ensureThread() error: %v", err)
	}
	if got := s.CurrentSessionID(); got != "thread-A" {
		t.Fatalf("CurrentSessionID() = %q, want thread-A", got)
	}
	s.stateMu.Lock()
	turnID := s.currentTurn
	pending := append([]string(nil), s.pendingMsgs...)
	s.stateMu.Unlock()
	if turnID != "turn-A" || len(pending) != 1 || pending[0] != "only A" {
		t.Fatalf("post-bind state = turn %q pending %#v, want only thread-A notifications", turnID, pending)
	}
}

func TestAppServerSession_ThreadResumePrebindsBeforeResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		transport:        appServerTransportProcess,
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		cancel:           cancel,
		stdin:            stdin,
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}

	done := make(chan error, 1)
	go func() { done <- s.ensureThread("thread-A") }()
	request := waitForAppServerClientRequest(t, stdin, "thread/resume")
	if got := s.CurrentSessionID(); got != "thread-A" {
		t.Fatalf("CurrentSessionID() before response = %q, want expected thread-A", got)
	}

	s.handleNotification("turn/started", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-B", "turn": map[string]any{"id": "turn-B"},
	}))
	s.handleNotification("turn/started", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-A", "turn": map[string]any{"id": "turn-A"},
	}))
	s.handleResponse(rpcResponseEnvelope{
		ID:     request.ID,
		Result: mustMarshalAppServerTest(t, map[string]any{"thread": map[string]any{"id": "thread-A"}}),
	})

	if err := <-done; err != nil {
		t.Fatalf("ensureThread() error: %v", err)
	}
	s.stateMu.Lock()
	turnID := s.currentTurn
	s.stateMu.Unlock()
	if turnID != "turn-A" {
		t.Fatalf("current turn = %q, want only matching pre-response notification", turnID)
	}
}

func TestAppServerSession_TurnStartResponseClaimsImmediateServerRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		transport:        appServerTransportProcess,
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		cancel:           cancel,
		stdin:            stdin,
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}
	s.alive.Store(true)
	s.threadID.Store("thread-A")

	done := make(chan error, 1)
	go func() { done <- s.Send("hello", "message-A", nil, nil) }()
	request := waitForAppServerClientRequest(t, stdin, "turn/start")
	if got := stringMapValue(request.Params, "clientUserMessageId"); got != "message-A" {
		t.Fatalf("turn/start clientUserMessageId = %q, want message-A", got)
	}
	s.handleResponse(rpcResponseEnvelope{
		ID:     request.ID,
		Result: mustMarshalAppServerTest(t, map[string]any{"turn": map[string]any{"id": "turn-A"}}),
	})
	s.stateMu.Lock()
	ownedTurn := s.ownedTurn
	s.stateMu.Unlock()
	if ownedTurn != "turn-A" {
		t.Fatalf("owned turn immediately after response = %q, want turn-A", ownedTurn)
	}
	// The reader can receive this request before the Send goroutine wakes up.
	// Ownership must therefore be established synchronously in handleResponse.
	s.handleServerRequest(serverRequestProbe(t, `"approval-1"`, "item/commandExecution/requestApproval", map[string]any{
		"threadId": "thread-A",
		"turnId":   "turn-A",
		"itemId":   "command-A",
		"command":  "pwd",
	}))

	event := waitForAppServerEvent(t, s.events)
	if event.Type != core.EventPermissionRequest || event.RequestID != `"approval-1"` {
		t.Fatalf("immediate server request event = %#v", event)
	}
	if err := s.RespondPermission(event.RequestID, core.PermissionResult{Behavior: "deny"}); err != nil {
		t.Fatalf("RespondPermission() error: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Send() error: %v", err)
	}
}

func TestAppServerSession_SendWithCollaborationModeUsesAtomicTurnOverride(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		transport:        appServerTransportProcess,
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		cancel:           cancel,
		stdin:            stdin,
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
		model:            "gpt-test",
		effort:           "high",
	}
	s.alive.Store(true)
	s.threadID.Store("thread-A")

	done := make(chan error, 1)
	go func() { done <- s.SendWithCollaborationMode("implement the plan", "message-plan", nil, nil, "default") }()
	request := waitForAppServerClientRequest(t, stdin, "turn/start")
	override, ok := request.Params["collaborationMode"].(map[string]any)
	if !ok || override["mode"] != "default" {
		t.Fatalf("collaborationMode = %#v", request.Params["collaborationMode"])
	}
	settings, ok := override["settings"].(map[string]any)
	if !ok || settings["model"] != "gpt-test" || settings["reasoning_effort"] != "high" {
		t.Fatalf("collaborationMode settings = %#v", override["settings"])
	}
	if value, exists := settings["developer_instructions"]; !exists || value != nil {
		t.Fatalf("developer_instructions = %#v, want explicit null", settings)
	}
	s.handleResponse(rpcResponseEnvelope{
		ID:     request.ID,
		Result: mustMarshalAppServerTest(t, map[string]any{"turn": map[string]any{"id": "turn-plan-execution"}}),
	})
	if err := <-done; err != nil {
		t.Fatalf("SendWithCollaborationMode() error: %v", err)
	}
}

func TestAppServerSession_TurnStartResponseDoesNotResurrectCompletedTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		transport:        appServerTransportProcess,
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		cancel:           cancel,
		stdin:            stdin,
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}
	s.alive.Store(true)
	s.threadID.Store("thread-A")

	done := make(chan error, 1)
	go func() { done <- s.Send("hello", "message-A", nil, nil) }()
	request := waitForAppServerClientRequest(t, stdin, "turn/start")
	s.handleNotification("turn/started", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-A",
		"turn":     map[string]any{"id": "turn-A", "status": "inProgress"},
	}))
	s.handleNotification("turn/completed", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-A",
		"turn":     map[string]any{"id": "turn-A", "status": "completed"},
	}))
	s.handleResponse(rpcResponseEnvelope{
		ID:     request.ID,
		Result: mustMarshalAppServerTest(t, map[string]any{"turn": map[string]any{"id": "turn-A"}}),
	})

	if err := <-done; err != nil {
		t.Fatalf("Send() error: %v", err)
	}
	s.stateMu.Lock()
	currentTurn := s.currentTurn
	ownedTurn := s.ownedTurn
	s.stateMu.Unlock()
	if currentTurn != "" || ownedTurn != "" {
		t.Fatalf("completed turn resurrected: current=%q owned=%q", currentTurn, ownedTurn)
	}
}

func TestAppServerSession_NewThreadNotificationBufferIsBoundedAndExpires(t *testing.T) {
	s := newIsolatedAppServerTestSession("", "", 1)
	s.beginNotificationBuffer()
	for i := 0; i < appServerNotificationBufferMax+10; i++ {
		s.handleNotification("turn/started", mustMarshalAppServerTest(t, map[string]any{
			"threadId": fmt.Sprintf("thread-%d", i),
			"turn":     map[string]any{"id": fmt.Sprintf("turn-%d", i)},
		}))
	}
	if got := len(s.bufferedNotifications); got != appServerNotificationBufferMax {
		t.Fatalf("buffered notifications = %d, want cap %d", got, appServerNotificationBufferMax)
	}

	s.bindingMu.Lock()
	s.bufferNotificationsTo = time.Now().Add(-time.Millisecond)
	s.bindingMu.Unlock()
	s.handleNotification("turn/started", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-expired", "turn": map[string]any{"id": "turn-expired"},
	}))
	if got := len(s.bufferedNotifications); got != 0 {
		t.Fatalf("expired buffer retained %d notifications", got)
	}
}

func TestAppServerSession_ServerRequestsRequireThreadTurnOwner(t *testing.T) {
	methods := []string{
		"item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval",
		"item/tool/requestUserInput",
		"item/tool/call",
		"mcpServer/elicitation/request",
	}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stdin := &lockedWriteCloser{}
			s := &appServerSession{
				events:           make(chan core.Event, 4),
				ctx:              ctx,
				pendingApprovals: make(map[string]chan core.PermissionResult),
				stdin:            stdin,
			}
			s.threadID.Store("thread-A")
			s.currentTurn = "turn-A"
			s.ownedTurn = "turn-A"

			badParams := []map[string]any{
				{"threadId": "thread-B", "turnId": "turn-B"},
				{"threadId": "thread-A", "turnId": "turn-B"},
				{"turnId": "turn-A"},
				{"threadId": 42, "turnId": "turn-A"},
			}
			for i, params := range badParams {
				s.handleServerRequest(serverRequestProbe(t, fmt.Sprintf("%d", i+1), method, params))
			}
			s.stateMu.Lock()
			s.ownedTurn = ""
			s.stateMu.Unlock()
			s.handleServerRequest(serverRequestProbe(t, "99", method, map[string]any{
				"threadId": "thread-A",
				"turnId":   "turn-A",
			}))

			assertNoAppServerEvent(t, s.events)
			if got := stdin.String(); got != "" {
				t.Fatalf("unowned requests wrote responses: %q", got)
			}
			s.approvalsMu.Lock()
			pending := len(s.pendingApprovals)
			s.approvalsMu.Unlock()
			if pending != 0 {
				t.Fatalf("unowned requests created %d pending approvals", pending)
			}
		})
	}
}

func TestAppServerSession_DaemonWriterLeaseAllowsOnlyOneOwner(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	s1 := newIsolatedAppServerTestSession("", "", 1)
	s1.transport = appServerTransportDaemon
	s1.socketPath = socketPath
	s2 := newIsolatedAppServerTestSession("", "", 1)
	s2.transport = appServerTransportDaemon
	s2.socketPath = socketPath

	if err := s1.bindThread("thread-A"); err != nil {
		t.Fatalf("first bindThread() error: %v", err)
	}
	if err := s2.bindThread("thread-A"); !errors.Is(err, core.ErrAgentSessionWriterBusy) {
		t.Fatalf("second bindThread() error = %v, want ErrAgentSessionWriterBusy", err)
	}
	s1.releaseWriterLease()
	if err := s2.bindThread("thread-A"); err != nil {
		t.Fatalf("bindThread() after release error: %v", err)
	}
	s2.releaseWriterLease()

	dead := newIsolatedAppServerTestSession("", "", 1)
	dead.transport = appServerTransportDaemon
	dead.socketPath = socketPath
	dead.alive.Store(false)
	if err := dead.bindThread("thread-A"); err == nil {
		t.Fatal("dead daemon connection unexpectedly acquired a writer lease")
	}
	probe := newIsolatedAppServerTestSession("", "", 1)
	probe.transport = appServerTransportDaemon
	probe.socketPath = socketPath
	if err := probe.bindThread("thread-A"); err != nil {
		t.Fatalf("live probe bindThread() after dead connection error: %v", err)
	}
	probe.releaseWriterLease()
}

func TestAppServerSession_ReadOnlyBindingDoesNotHoldWriterLease(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	reader := newIsolatedAppServerTestSession("", "", 1)
	reader.transport = appServerTransportDaemon
	reader.socketPath = socketPath
	writer := newIsolatedAppServerTestSession("", "", 1)
	writer.transport = appServerTransportDaemon
	writer.socketPath = socketPath

	if err := reader.bindReadOnlyThread("thread-A"); err != nil {
		t.Fatalf("bindReadOnlyThread() error: %v", err)
	}
	if reader.ownsWriterLease() {
		t.Fatal("read-only observer unexpectedly owns the writer lease")
	}
	if err := writer.bindThread("thread-A"); err != nil {
		t.Fatalf("writer bindThread() blocked by observer: %v", err)
	}
	writer.releaseWriterLease()
}

func TestAppServerSession_FakeSharedDaemonRoutesNotificationsAndServerRequestsToOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are not available on Windows")
	}
	daemon := newFakeSharedAppServerDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startSession := func(threadID string) *appServerSession {
		t.Helper()
		s, err := newAppServerSession(
			ctx,
			appServerTransportDaemon,
			"",
			daemon.socketPath,
			t.TempDir(),
			"",
			"",
			"suggest",
			threadID,
			"",
			"",
			nil,
			"",
			"",
			"",
		)
		if err != nil {
			t.Fatalf("start %s session: %v", threadID, err)
		}
		return s
	}

	sessionA := startSession("thread-A")
	defer sessionA.Close()
	sessionB := startSession("thread-B")
	defer sessionB.Close()
	daemon.waitForClients(t, 2)

	for _, threadID := range []string{"thread-A", "thread-B"} {
		turnID := "notification-turn-" + threadID
		daemon.broadcast(t, "turn/started", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "inProgress"},
		})
		daemon.broadcast(t, "item/started", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "commandExecution", "command": "tool-" + threadID},
		})
		daemon.broadcast(t, "item/completed", map[string]any{
			"threadId": threadID,
			"turnId":   turnID,
			"item":     map[string]any{"type": "agentMessage", "text": "reply-" + threadID},
		})
		daemon.broadcast(t, "turn/completed", map[string]any{
			"threadId": threadID,
			"turn":     map[string]any{"id": turnID, "status": "completed"},
		})
	}

	assertFakeDaemonSessionEvents(t, sessionA, "thread-A", "thread-B")
	assertFakeDaemonSessionEvents(t, sessionB, "thread-B", "thread-A")

	if err := sessionA.Send("owner A", "message-A", nil, nil); err != nil {
		t.Fatalf("start owned turn A: %v", err)
	}
	if err := sessionB.Send("owner B", "message-B", nil, nil); err != nil {
		t.Fatalf("start owned turn B: %v", err)
	}
	waitForAppServerTurn(t, sessionA, "server-turn-thread-A")
	waitForAppServerTurn(t, sessionB, "server-turn-thread-B")

	requests := []struct {
		method       string
		params       map[string]any
		expectsEvent bool
	}{
		{
			method:       "item/commandExecution/requestApproval",
			params:       map[string]any{"itemId": "command-1", "startedAtMs": 1, "command": "pwd"},
			expectsEvent: true,
		},
		{
			method:       "item/fileChange/requestApproval",
			params:       map[string]any{"itemId": "patch-1", "startedAtMs": 1, "reason": "update file"},
			expectsEvent: true,
		},
		{
			method:       "item/permissions/requestApproval",
			params:       map[string]any{"itemId": "permissions-1", "startedAtMs": 1, "cwd": "/tmp", "permissions": map[string]any{}},
			expectsEvent: true,
		},
		{
			method: "item/tool/requestUserInput",
			params: map[string]any{
				"itemId": "question-1", "isBlocking": true,
				"questions": []any{map[string]any{
					"id": "choice", "header": "Choice", "question": "Choose one",
					"options": []any{map[string]any{"label": "A", "description": "Option A"}},
				}},
			},
			expectsEvent: true,
		},
		{
			method: "item/tool/call",
			params: map[string]any{"callId": "dynamic-1", "tool": "missing_tool", "arguments": map[string]any{}},
		},
		{
			method: "mcpServer/elicitation/request",
			params: map[string]any{"serverName": "test-mcp"},
		},
	}

	requestID := 100
	for _, threadID := range []string{"thread-A", "thread-B"} {
		owner := sessionA
		nonOwner := sessionB
		if threadID == "thread-B" {
			owner, nonOwner = sessionB, sessionA
		}
		for _, request := range requests {
			requestID++
			params := make(map[string]any, len(request.params)+2)
			for key, value := range request.params {
				params[key] = value
			}
			params["threadId"] = threadID
			params["turnId"] = "server-turn-" + threadID
			daemon.broadcastRequest(t, requestID, request.method, params)

			if request.expectsEvent {
				event := waitForAppServerEvent(t, owner.Events())
				if event.Type != core.EventPermissionRequest || event.RequestID != fmt.Sprintf("%d", requestID) {
					t.Fatalf("%s owner event = %#v", request.method, event)
				}
				if err := owner.RespondPermission(event.RequestID, core.PermissionResult{Behavior: "deny"}); err != nil {
					t.Fatalf("respond to %s: %v", request.method, err)
				}
			}
			assertNoAppServerEvent(t, nonOwner.Events())

			response := daemon.waitForResponse(t, requestID)
			if response.threadID != threadID {
				t.Fatalf("%s response came from %q, want owner %q", request.method, response.threadID, threadID)
			}
			daemon.assertNoResponse(t, 30*time.Millisecond)
		}
	}

	disconnectedThread := sessionA.CurrentSessionID()
	daemon.disconnectThread(t, disconnectedThread)
	waitForAppServerSessionDead(t, sessionA)
	disconnectEvent := waitForAppServerEvent(t, sessionA.Events())
	if disconnectEvent.Type != core.EventError {
		t.Fatalf("daemon disconnect event = %#v, want EventError", disconnectEvent)
	}
	replacement := startSession(disconnectedThread)
	defer replacement.Close()
	if got := replacement.CurrentSessionID(); got != disconnectedThread {
		t.Fatalf("replacement session ID = %q, want %q", got, disconnectedThread)
	}

	daemon.assertHealthy(t)
}

func newIsolatedAppServerTestSession(threadID, turnID string, eventBuffer int) *appServerSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &appServerSession{
		events:           make(chan core.Event, eventBuffer),
		ctx:              ctx,
		cancel:           cancel,
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}
	s.alive.Store(true)
	if threadID != "" {
		s.threadID.Store(threadID)
	}
	s.currentTurn = turnID
	s.ownedTurn = turnID
	return s
}

func mustMarshalAppServerTest(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal app-server test payload: %v", err)
	}
	return raw
}

func assertAppServerStateUnchanged(t *testing.T, s *appServerSession, wantTurn string, wantPending []string, wantUsedTokens int) {
	t.Helper()
	s.stateMu.Lock()
	gotTurn := s.currentTurn
	gotOwnedTurn := s.ownedTurn
	gotPending := append([]string(nil), s.pendingMsgs...)
	s.stateMu.Unlock()
	if gotTurn != wantTurn {
		t.Fatalf("current turn = %q, want %q", gotTurn, wantTurn)
	}
	if gotOwnedTurn != wantTurn {
		t.Fatalf("owned turn = %q, want %q", gotOwnedTurn, wantTurn)
	}
	if len(gotPending) != len(wantPending) {
		t.Fatalf("pending messages = %#v, want %#v", gotPending, wantPending)
	}
	for i := range wantPending {
		if gotPending[i] != wantPending[i] {
			t.Fatalf("pending messages = %#v, want %#v", gotPending, wantPending)
		}
	}
	usage := s.cachedContextUsage()
	if wantUsedTokens == 0 {
		if usage != nil {
			t.Fatalf("context usage = %#v, want nil", usage)
		}
		return
	}
	if usage == nil || usage.UsedTokens != wantUsedTokens {
		t.Fatalf("context usage = %#v, want used tokens %d", usage, wantUsedTokens)
	}
}

func assertNoAppServerEvent(t *testing.T, events <-chan core.Event) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("unexpected app-server event: %#v", event)
	default:
	}
}

type fakeSharedAppServerResponse struct {
	threadID string
	id       int
	payload  map[string]json.RawMessage
}

type fakeSharedAppServerClient struct {
	daemon *fakeSharedAppServerDaemon
	conn   *websocket.Conn

	writeMu     sync.Mutex
	threadID    string
	resumeCalls int
}

type fakeSharedAppServerSteer struct {
	ThreadID            string           `json:"threadId"`
	ExpectedTurnID      string           `json:"expectedTurnId"`
	Input               []map[string]any `json:"input"`
	ClientUserMessageID string           `json:"clientUserMessageId"`
}

type fakeSharedAppServerDaemon struct {
	socketPath string
	server     *http.Server
	listener   net.Listener

	mu      sync.Mutex
	clients []*fakeSharedAppServerClient

	conversationCwd    string
	conversationStatus string
	conversationFlags  []string
	conversationTurns  map[string][]map[string]any
	interrupts         chan appServerThreadIdentity
	steers             chan fakeSharedAppServerSteer

	responses chan fakeSharedAppServerResponse
	errors    chan error
	done      chan struct{}
}

func newFakeSharedAppServerDaemon(t *testing.T) *fakeSharedAppServerDaemon {
	t.Helper()
	socketFile, err := os.CreateTemp("", "cc-shared-*.sock")
	if err != nil {
		t.Fatalf("reserve fake app-server socket path: %v", err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatalf("close fake app-server socket file: %v", err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("remove fake app-server socket file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on fake app-server socket: %v", err)
	}
	d := &fakeSharedAppServerDaemon{
		socketPath:        socketPath,
		listener:          listener,
		responses:         make(chan fakeSharedAppServerResponse, 32),
		errors:            make(chan error, 16),
		done:              make(chan struct{}),
		conversationTurns: make(map[string][]map[string]any),
		interrupts:        make(chan appServerThreadIdentity, 8),
		steers:            make(chan fakeSharedAppServerSteer, 8),
	}
	d.server = &http.Server{Handler: http.HandlerFunc(d.serveHTTP)}
	go func() {
		if err := d.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.reportError(fmt.Errorf("fake app-server serve: %w", err))
		}
	}()
	t.Cleanup(func() {
		close(d.done)
		_ = d.server.Close()
		_ = d.listener.Close()
	})
	return d
}

func (d *fakeSharedAppServerDaemon) serveHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		d.reportError(fmt.Errorf("fake app-server upgrade: %w", err))
		return
	}
	client := &fakeSharedAppServerClient{daemon: d, conn: conn}
	d.mu.Lock()
	d.clients = append(d.clients, client)
	d.mu.Unlock()
	client.readLoop()
}

func (c *fakeSharedAppServerClient) readLoop() {
	defer c.conn.Close()
	for {
		var message map[string]json.RawMessage
		if err := c.conn.ReadJSON(&message); err != nil {
			select {
			case <-c.daemon.done:
				return
			default:
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return
			}
			c.daemon.reportError(fmt.Errorf("fake app-server read: %w", err))
			return
		}

		rawID, hasID := message["id"]
		rawMethod, hasMethod := message["method"]
		if hasID && !hasMethod {
			var id int
			if err := json.Unmarshal(rawID, &id); err != nil {
				c.daemon.reportError(fmt.Errorf("decode client response id: %w", err))
				continue
			}
			c.daemon.mu.Lock()
			threadID := c.threadID
			c.daemon.mu.Unlock()
			c.daemon.responses <- fakeSharedAppServerResponse{threadID: threadID, id: id, payload: message}
			continue
		}
		if !hasMethod {
			continue
		}

		var method string
		if err := json.Unmarshal(rawMethod, &method); err != nil {
			c.daemon.reportError(fmt.Errorf("decode client method: %w", err))
			continue
		}
		if !hasID {
			continue
		}
		switch method {
		case "initialize":
			c.writeResponse(rawID, map[string]any{"protocolVersion": "2"})
		case "thread/resume":
			var params struct {
				ThreadID string `json:"threadId"`
			}
			if err := json.Unmarshal(message["params"], &params); err != nil || params.ThreadID == "" {
				c.daemon.reportError(fmt.Errorf("decode thread/resume params: %v", err))
				continue
			}
			c.daemon.mu.Lock()
			c.threadID = params.ThreadID
			c.resumeCalls++
			c.daemon.mu.Unlock()
			c.writeResponse(rawID, map[string]any{
				"thread": map[string]any{"id": params.ThreadID},
			})
		case "turn/start":
			var params struct {
				ThreadID string `json:"threadId"`
			}
			if err := json.Unmarshal(message["params"], &params); err != nil || params.ThreadID == "" {
				c.daemon.reportError(fmt.Errorf("decode turn/start params: %v", err))
				continue
			}
			c.daemon.mu.Lock()
			boundThread := c.threadID
			c.daemon.mu.Unlock()
			if params.ThreadID != boundThread {
				c.daemon.reportError(fmt.Errorf("turn/start thread = %q, client bound to %q", params.ThreadID, boundThread))
				continue
			}
			c.writeResponse(rawID, map[string]any{
				"turn": map[string]any{"id": "server-turn-" + params.ThreadID},
			})
		case "account/rateLimits/read":
			c.writeResponse(rawID, map[string]any{"rateLimitsByLimitId": map[string]any{}})
		case "thread/read":
			var params struct {
				ThreadID     string `json:"threadId"`
				IncludeTurns bool   `json:"includeTurns"`
			}
			if err := json.Unmarshal(message["params"], &params); err != nil || params.ThreadID == "" {
				c.daemon.reportError(fmt.Errorf("decode thread/read params: %v", err))
				continue
			}
			c.daemon.mu.Lock()
			cwd := c.daemon.conversationCwd
			status := c.daemon.conversationStatus
			flags := append([]string(nil), c.daemon.conversationFlags...)
			turns := append([]map[string]any(nil), c.daemon.conversationTurns[params.ThreadID]...)
			c.daemon.mu.Unlock()
			if status == "" {
				status = "idle"
			}
			statusValue := map[string]any{"type": status}
			if status == "active" {
				statusValue["activeFlags"] = flags
			}
			if !params.IncludeTurns {
				turns = nil
			}
			c.writeResponse(rawID, map[string]any{"thread": map[string]any{
				"id": params.ThreadID, "cwd": cwd, "status": statusValue, "turns": turns,
			}})
		case "thread/turns/list":
			var params struct {
				ThreadID string `json:"threadId"`
				Limit    int    `json:"limit"`
				Cursor   string `json:"cursor"`
			}
			if err := json.Unmarshal(message["params"], &params); err != nil || params.ThreadID == "" {
				c.daemon.reportError(fmt.Errorf("decode thread/turns/list params: %v", err))
				continue
			}
			c.daemon.mu.Lock()
			turns := append([]map[string]any(nil), c.daemon.conversationTurns[params.ThreadID]...)
			c.daemon.mu.Unlock()
			start := 0
			if params.Cursor != "" {
				start, _ = strconv.Atoi(params.Cursor)
			}
			if start > len(turns) {
				start = len(turns)
			}
			end := len(turns)
			if params.Limit > 0 && start+params.Limit < end {
				end = start + params.Limit
			}
			var nextCursor any
			if end < len(turns) {
				nextCursor = strconv.Itoa(end)
			}
			c.writeResponse(rawID, map[string]any{"data": turns[start:end], "nextCursor": nextCursor, "backwardsCursor": nil})
		case "turn/steer":
			var params fakeSharedAppServerSteer
			if err := json.Unmarshal(message["params"], &params); err != nil || params.ThreadID == "" || params.ExpectedTurnID == "" {
				c.daemon.reportError(fmt.Errorf("decode turn/steer params: %v", err))
				continue
			}
			c.daemon.steers <- params
			c.writeResponse(rawID, map[string]any{"turnId": params.ExpectedTurnID})
		case "turn/interrupt":
			var params appServerThreadIdentity
			if err := json.Unmarshal(message["params"], &params); err != nil || params.ThreadID == "" || params.TurnID == "" {
				c.daemon.reportError(fmt.Errorf("decode turn/interrupt params: %v", err))
				continue
			}
			c.daemon.interrupts <- params
			c.writeResponse(rawID, map[string]any{})
		default:
			c.daemon.reportError(fmt.Errorf("unexpected fake app-server client method %q", method))
		}
	}
}

func (d *fakeSharedAppServerDaemon) setConversation(cwd, threadID, status string, flags []string, turnsDescending []map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.conversationCwd = cwd
	d.conversationStatus = status
	d.conversationFlags = append([]string(nil), flags...)
	d.conversationTurns[threadID] = append([]map[string]any(nil), turnsDescending...)
}

func (c *fakeSharedAppServerClient) writeResponse(id json.RawMessage, result any) {
	if err := c.writeJSON(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		c.daemon.reportError(err)
	}
}

func (c *fakeSharedAppServerClient) writeJSON(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.WriteJSON(value); err != nil {
		return fmt.Errorf("fake app-server write: %w", err)
	}
	return nil
}

func (d *fakeSharedAppServerDaemon) reportError(err error) {
	if err == nil {
		return
	}
	select {
	case <-d.done:
		return
	default:
	}
	select {
	case d.errors <- err:
	default:
	}
}

func (d *fakeSharedAppServerDaemon) waitForClients(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		got := len(d.clients)
		d.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake daemon did not receive %d clients", want)
}

func (d *fakeSharedAppServerDaemon) snapshotClients(t *testing.T) []*fakeSharedAppServerClient {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.clients) == 0 {
		t.Fatal("fake daemon has no connected clients")
	}
	return append([]*fakeSharedAppServerClient(nil), d.clients...)
}

func (d *fakeSharedAppServerDaemon) broadcast(t *testing.T, method string, params any) {
	t.Helper()
	message := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	for _, client := range d.snapshotClients(t) {
		if err := client.writeJSON(message); err != nil {
			t.Fatalf("broadcast %s: %v", method, err)
		}
	}
}

func (d *fakeSharedAppServerDaemon) broadcastRequest(t *testing.T, id int, method string, params any) {
	t.Helper()
	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	for _, client := range d.snapshotClients(t) {
		if err := client.writeJSON(message); err != nil {
			t.Fatalf("broadcast request %s: %v", method, err)
		}
	}
}

func (d *fakeSharedAppServerDaemon) disconnectThread(t *testing.T, threadID string) {
	t.Helper()
	var target *fakeSharedAppServerClient
	d.mu.Lock()
	for _, client := range d.clients {
		if client.threadID == threadID {
			target = client
			break
		}
	}
	d.mu.Unlock()
	if target == nil {
		t.Fatalf("fake daemon has no client for thread %q", threadID)
	}

	target.writeMu.Lock()
	err := target.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "test disconnect"),
		time.Now().Add(time.Second),
	)
	target.writeMu.Unlock()
	if err != nil {
		t.Fatalf("disconnect thread %q: %v", threadID, err)
	}
}

func (d *fakeSharedAppServerDaemon) waitForResponse(t *testing.T, id int) fakeSharedAppServerResponse {
	t.Helper()
	for {
		select {
		case response := <-d.responses:
			if response.id != id {
				t.Fatalf("fake daemon response id = %d, want %d", response.id, id)
			}
			return response
		case err := <-d.errors:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for response %d", id)
		}
	}
}

func (d *fakeSharedAppServerDaemon) assertNoResponse(t *testing.T, duration time.Duration) {
	t.Helper()
	select {
	case response := <-d.responses:
		t.Fatalf("unexpected duplicate/unowned response: id=%d thread=%q payload=%v", response.id, response.threadID, response.payload)
	case err := <-d.errors:
		t.Fatal(err)
	case <-time.After(duration):
	}
}

func (d *fakeSharedAppServerDaemon) assertHealthy(t *testing.T) {
	t.Helper()
	select {
	case err := <-d.errors:
		t.Fatal(err)
	default:
	}
}

func assertFakeDaemonSessionEvents(t *testing.T, session *appServerSession, ownThread, otherThread string) {
	t.Helper()
	for i := 0; i < 4; i++ {
		event := waitForAppServerEvent(t, session.Events())
		joined := strings.Join([]string{event.Content, event.ToolInput, event.ToolResult, event.SessionID}, "\n")
		if strings.Contains(joined, otherThread) {
			t.Fatalf("fake daemon leaked %s into %s: %#v", otherThread, ownThread, event)
		}
		if event.Type == core.EventResult && event.SessionID != ownThread {
			t.Fatalf("result SessionID = %q, want %q", event.SessionID, ownThread)
		}
	}
	assertNoAppServerEvent(t, session.Events())
}

func waitForAppServerEvent(t *testing.T, events <-chan core.Event) core.Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for app-server event")
		return core.Event{}
	}
}

func waitForAppServerTurn(t *testing.T, session *appServerSession, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session.stateMu.Lock()
		got := session.currentTurn
		session.stateMu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("current turn did not become %q", want)
}

func waitForAppServerSessionDead(t *testing.T, session *appServerSession) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !session.Alive() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("app-server session remained alive after daemon disconnect")
}

func TestAppServerSession_RequestTimeoutIncludesBlockedStdinWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := newBlockingWriteCloser()
	defer func() { _ = stdin.Close() }()

	s := &appServerSession{
		ctx:     ctx,
		cancel:  cancel,
		events:  make(chan core.Event),
		stdin:   stdin,
		pending: make(map[int64]chan rpcResponseEnvelope),
	}

	done := make(chan error, 1)
	go func() {
		var out map[string]any
		done <- s.requestWithTimeout("turn/start", map[string]any{
			"input": strings.Repeat("x", 1024),
		}, &out, 25*time.Millisecond)
	}()

	select {
	case <-stdin.started:
	case <-time.After(time.Second):
		t.Fatal("request did not attempt to write to stdin")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("requestWithTimeout returned nil, want write timeout")
		}
		if !strings.Contains(err.Error(), "turn/start") || !strings.Contains(err.Error(), "write timed out") {
			t.Fatalf("error = %q, want turn/start write timeout", err.Error())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("requestWithTimeout did not return while stdin write was blocked")
	}

	if !stdin.Closed() {
		t.Fatal("blocked stdin was not closed after timeout")
	}
}

func TestMapAppServerRateLimits_PrefersMultiBucketView(t *testing.T) {
	report := mapAppServerRateLimits(appServerRateLimitsResponse{
		RateLimits: appServerRateLimitSnapshot{
			LimitID:  "legacy",
			PlanType: "team",
			Primary:  &appServerRateLimitWindow{UsedPercent: 99, WindowDurationMins: 15},
		},
		RateLimitsByLimitID: map[string]appServerRateLimitSnapshot{
			"codex": {
				LimitID:   "codex",
				LimitName: "Codex",
				PlanType:  "team",
				Primary:   &appServerRateLimitWindow{UsedPercent: 10, WindowDurationMins: 15},
			},
			"codex_other": {
				LimitID:  "codex_other",
				PlanType: "team",
				Primary:  &appServerRateLimitWindow{UsedPercent: 20, WindowDurationMins: 60},
			},
		},
	})

	if report.Plan != "team" {
		t.Fatalf("plan = %q, want team", report.Plan)
	}
	if len(report.Buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(report.Buckets))
	}
	if report.Buckets[0].Name != "Codex" {
		t.Fatalf("first bucket = %q, want Codex", report.Buckets[0].Name)
	}
	if report.Buckets[1].Name != "codex_other" {
		t.Fatalf("second bucket = %q, want codex_other", report.Buckets[1].Name)
	}
}

func TestAppServerSession_HandleRequestUserInputEmitsAskQuestion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		pendingApprovals: make(map[string]chan core.PermissionResult),
		permissionDone:   make(chan string, 4),
		stdin:            stdin,
	}
	s.threadID.Store("thread-1")
	s.currentTurn = "turn-1"
	s.ownedTurn = "turn-1"

	s.handleServerRequest(serverRequestProbe(t, `"rui-1"`, "item/tool/requestUserInput", map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"itemId":   "call-1",
		"questions": []any{
			map[string]any{
				"id":       "database",
				"header":   "Database",
				"question": "Which database should we use?",
				"isOther":  true,
				"isSecret": true,
				"options": []any{
					map[string]any{"label": "Postgres", "description": "Use the existing relational database"},
					map[string]any{"label": "SQLite", "description": "Keep it embedded"},
				},
			},
		},
	}))

	var event core.Event
	select {
	case event = <-s.events:
	case <-time.After(time.Second):
		t.Fatal("expected AskUserQuestion event")
	}
	if event.Type != core.EventPermissionRequest {
		t.Fatalf("event type = %s, want %s", event.Type, core.EventPermissionRequest)
	}
	if event.ToolName != "AskUserQuestion" {
		t.Fatalf("tool name = %q, want AskUserQuestion", event.ToolName)
	}
	if event.RequestID != `"rui-1"` {
		t.Fatalf("request id = %q, want raw JSON id", event.RequestID)
	}
	if len(event.Questions) != 1 {
		t.Fatalf("questions = %d, want 1", len(event.Questions))
	}
	q := event.Questions[0]
	if q.Question != "Which database should we use?" || q.Header != "Database" || !q.Secret {
		t.Fatalf("question = %#v", q)
	}
	if len(q.Options) != 2 || q.Options[0].Label != "Postgres" || q.Options[1].Description != "Keep it embedded" {
		t.Fatalf("options = %#v", q.Options)
	}
	if stdin.String() != "" {
		t.Fatalf("request_user_input should not write before the answer, got %q", stdin.String())
	}
}

func TestAppServerSession_HandleRequestUserInputWritesCodexResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		pendingApprovals: make(map[string]chan core.PermissionResult),
		permissionDone:   make(chan string, 4),
		stdin:            stdin,
	}
	s.threadID.Store("thread-1")
	s.currentTurn = "turn-1"
	s.ownedTurn = "turn-1"

	s.handleServerRequest(serverRequestProbe(t, `"rui-2"`, "item/tool/requestUserInput", map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"itemId":   "call-2",
		"questions": []any{
			map[string]any{
				"id":       "database",
				"header":   "Database",
				"question": "Which database should we use?",
				"options": []any{
					map[string]any{"label": "Postgres", "description": "Use the existing relational database"},
					map[string]any{"label": "SQLite", "description": "Keep it embedded"},
				},
			},
		},
	}))

	var event core.Event
	select {
	case event = <-s.events:
	case <-time.After(time.Second):
		t.Fatal("expected AskUserQuestion event")
	}
	if err := s.RespondPermission(event.RequestID, core.PermissionResult{
		Behavior: "allow",
		UpdatedInput: map[string]any{
			"answers": map[string]any{
				"Which database should we use?": "Postgres",
			},
		},
	}); err != nil {
		t.Fatalf("RespondPermission() error = %v", err)
	}

	line := waitForWrittenJSONLine(t, stdin)
	var envelope struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Result  struct {
			Answers map[string]struct {
				Answers []string `json:"answers"`
			} `json:"answers"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("decode response %q: %v", line, err)
	}
	if envelope.JSONRPC != "2.0" || envelope.ID != "rui-2" {
		t.Fatalf("envelope = %#v", envelope)
	}
	got := envelope.Result.Answers["database"].Answers
	if len(got) != 1 || got[0] != "Postgres" {
		t.Fatalf("answers[database] = %#v, want [Postgres]", got)
	}
	s.handleNotification("serverRequest/resolved", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-1", "requestId": "rui-2",
	}))
	select {
	case requestID := <-s.PermissionResolutions():
		t.Fatalf("locally answered request leaked into external resolution side channel: %q", requestID)
	default:
	}
}

func TestAppServerSession_RequestUserInputResolvedByAnotherClientDoesNotReply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin := &lockedWriteCloser{}
	s := &appServerSession{
		events:           make(chan core.Event, 4),
		ctx:              ctx,
		pendingApprovals: make(map[string]chan core.PermissionResult),
		observedInputs:   make(map[string]appServerObservedRequestUserInput),
		permissionDone:   make(chan string, 4),
		stdin:            stdin,
	}
	s.threadID.Store("thread-1")
	s.currentTurn = "turn-1"
	s.ownedTurn = "turn-1"

	s.handleServerRequest(serverRequestProbe(t, `"rui-external"`, "item/tool/requestUserInput", map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"itemId":   "call-external",
		"questions": []any{map[string]any{
			"id": "database", "question": "Which database?",
			"options": []any{map[string]any{"label": "Postgres"}},
		}},
	}))
	select {
	case event := <-s.events:
		if event.Type != core.EventPermissionRequest || event.ThreadID != "thread-1" || event.TurnID != "turn-1" || event.ItemID != "call-external" {
			t.Fatalf("request event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("expected AskUserQuestion event")
	}

	s.handleNotification("serverRequest/resolved", mustMarshalAppServerTest(t, map[string]any{
		"threadId": "thread-1", "requestId": "rui-external",
	}))
	select {
	case requestID := <-s.PermissionResolutions():
		if requestID != `"rui-external"` {
			t.Fatalf("resolved request id = %q", requestID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission resolution side channel")
	}
	select {
	case event := <-s.events:
		if event.Type != core.EventPermissionResolved || event.RequestID != `"rui-external"` || event.ThreadID != "thread-1" {
			t.Fatalf("resolution event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission resolution event")
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if stdin.String() != "" {
			t.Fatalf("resolved request wrote a duplicate response: %q", stdin.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.RespondPermission(`"rui-external"`, core.PermissionResult{Behavior: "allow"}); err == nil {
		t.Fatal("stale resolved request unexpectedly accepted another answer")
	}
}

var _ interface {
	GetUsage(context.Context) (*core.UsageReport, error)
} = (*appServerSession)(nil)

var _ interface {
	GetContextUsage() *core.ContextUsage
} = (*appServerSession)(nil)

type lockedWriteCloser struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriteCloser) Close() error { return nil }

func (w *lockedWriteCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

var _ io.WriteCloser = (*lockedWriteCloser)(nil)

type blockingWriteCloser struct {
	started   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	isClosed bool
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	select {
	case <-w.started:
	default:
		close(w.started)
	}
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *blockingWriteCloser) Close() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.isClosed = true
		w.mu.Unlock()
		close(w.closed)
	})
	return nil
}

func (w *blockingWriteCloser) Closed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.isClosed
}

var _ io.WriteCloser = (*blockingWriteCloser)(nil)

func serverRequestProbe(t *testing.T, idJSON, method string, params any) map[string]json.RawMessage {
	t.Helper()
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	methodJSON, err := json.Marshal(method)
	if err != nil {
		t.Fatalf("marshal method: %v", err)
	}
	return map[string]json.RawMessage{
		"id":     json.RawMessage(idJSON),
		"method": methodJSON,
		"params": paramsJSON,
	}
}

type appServerTestClientRequest struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

func waitForAppServerClientRequest(t *testing.T, w *lockedWriteCloser, wantMethod string) appServerTestClientRequest {
	t.Helper()
	line := waitForWrittenJSONLine(t, w)
	var request appServerTestClientRequest
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		t.Fatalf("unmarshal client request %q: %v", line, err)
	}
	if request.Method != wantMethod {
		t.Fatalf("client request method = %q, want %q", request.Method, wantMethod)
	}
	return request
}

func waitForWrittenJSONLine(t *testing.T, w *lockedWriteCloser) string {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for JSON response, buffer=%q", w.String())
		case <-ticker.C:
			for _, line := range strings.Split(w.String(), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					return line
				}
			}
		}
	}
}
