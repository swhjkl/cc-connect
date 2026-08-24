package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
		stdin:            stdin,
	}

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
				"isSecret": false,
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
	if q.Question != "Which database should we use?" || q.Header != "Database" {
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
		stdin:            stdin,
	}

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
