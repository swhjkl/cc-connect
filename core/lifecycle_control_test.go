package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type closeoutAgentSession struct {
	id         string
	closeCalls int
}

type blockingListAgent struct {
	called  chan struct{}
	release chan struct{}
}

func (a *blockingListAgent) Name() string { return "stub" }
func (a *blockingListAgent) StartSession(context.Context, string) (AgentSession, error) {
	return &closeoutAgentSession{id: "thread-new"}, nil
}
func (a *blockingListAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	close(a.called)
	<-a.release
	return []AgentSessionInfo{{ID: "thread-new", Summary: "new"}}, nil
}
func (a *blockingListAgent) Stop() error { return nil }

func (s *closeoutAgentSession) Send(string, string, []ImageAttachment, []FileAttachment) error {
	return nil
}
func (s *closeoutAgentSession) RespondPermission(string, PermissionResult) error { return nil }
func (s *closeoutAgentSession) Events() <-chan Event                             { return make(chan Event) }
func (s *closeoutAgentSession) CurrentSessionID() string                         { return s.id }
func (s *closeoutAgentSession) Alive() bool                                      { return true }
func (s *closeoutAgentSession) Close() error                                     { s.closeCalls++; return nil }

func setupBusyCloseout(t *testing.T) (*Engine, *APIServer, string, string, *closeoutAgentSession) {
	t.Helper()
	tmp := t.TempDir()
	worktree := filepath.Join(tmp, "worktree")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	e := NewEngine("project", &stubAgent{}, []Platform{&stubPlatformEngine{n: "feishu"}}, "", LangEnglish)
	e.SetMultiWorkspace(tmp, filepath.Join(tmp, "bindings.json"))
	sessionKey := "feishu:chat:user"
	e.workspaceBindings.Bind("project:project", "feishu:chat", "", worktree)
	ws := e.workspacePool.GetOrCreate(worktree)
	ws.mu.Lock()
	ws.agent = e.agent
	ws.sessions = NewSessionManager("")
	active := ws.sessions.GetOrCreateActive(sessionKey)
	active.SetAgentSessionID("thread-exact", "stub")
	if !active.TryLock() {
		t.Fatal("expected session lock")
	}
	ws.mu.Unlock()
	live := &closeoutAgentSession{id: "thread-exact"}
	e.interactiveStates[worktree+":"+sessionKey] = &interactiveState{agentSession: live, currentSessionKey: sessionKey, workspaceDir: worktree}
	s := &APIServer{mux: http.NewServeMux(), engines: map[string]*Engine{"project": e}}
	s.mux.HandleFunc("/lifecycle/workspace/unbind", s.handleWorkspaceUnbind)
	return e, s, worktree, sessionKey, live
}

func postCloseout(t *testing.T, s *APIServer, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/lifecycle/workspace/unbind", bytes.NewReader(b))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w
}

func TestLifecycleWorkspaceUnbind_BusyExactCASPreservesActiveTurn(t *testing.T) {
	e, server, worktree, sessionKey, live := setupBusyCloseout(t)
	w := postCloseout(t, server, map[string]string{"project": "project", "session": sessionKey, "worktree": worktree, "expected_agent_session_id": "thread-exact"})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var envelope struct {
		SchemaVersion int  `json:"schema_version"`
		OK            bool `json:"ok"`
		Result        struct {
			CloseoutGuard CloseoutGuard `json:"closeout_guard"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || !envelope.OK || !envelope.Result.CloseoutGuard.Verified || !envelope.Result.CloseoutGuard.ActiveTurnPreserved {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
	if live.closeCalls != 0 {
		t.Fatalf("active process was closed %d times", live.closeCalls)
	}
	if e.workspaceBindings.LookupExact("project:project", "feishu:chat") != nil {
		t.Fatal("route was not removed")
	}
	if e.interactiveStates[worktree+":"+sessionKey] == nil {
		t.Fatal("active state was removed")
	}
}

func TestWorkspaceRoute_ExactExistingRouteIsIdempotentWhileBusy(t *testing.T) {
	e, _, worktree, sessionKey, live := setupBusyCloseout(t)
	result, err := e.WorkspaceRoute(sessionKey, worktree)
	if err != nil {
		t.Fatalf("exact busy route should be idempotent: %v", err)
	}
	if result.Changed || result.Status != "already_routed" || result.Worktree != worktree {
		t.Fatalf("unexpected route result: %#v", result)
	}
	if live.closeCalls != 0 {
		t.Fatal("idempotent route interrupted the active process")
	}
	if binding := e.workspaceBindings.LookupExact("project:project", "feishu:chat"); binding == nil || normalizeWorkspacePath(binding.Workspace) != worktree {
		t.Fatalf("idempotent route changed binding: %#v", binding)
	}
}

func TestWorkspaceRoute_NewRouteRemainsBlockedWhileTargetSessionBusy(t *testing.T) {
	e, _, worktree, sessionKey, _ := setupBusyCloseout(t)
	changed, err := e.workspaceBindings.UnbindCAS("project:project", "feishu:chat", worktree)
	if err != nil || !changed {
		t.Fatalf("remove existing route: changed=%v err=%v", changed, err)
	}
	if _, err := e.WorkspaceRoute(sessionKey, worktree); err == nil {
		t.Fatal("new route succeeded while target session was busy")
	}
	if binding := e.workspaceBindings.LookupExact("project:project", "feishu:chat"); binding != nil {
		t.Fatalf("busy route unexpectedly created binding: %#v", binding)
	}
}

func TestSessionsAttach_SerializesRouteValidationAndPersistenceWithUnbind(t *testing.T) {
	e, _, worktree, sessionKey, _ := setupBusyCloseout(t)
	ws := e.workspacePool.Get(worktree)
	ws.mu.Lock()
	active := ws.sessions.ActiveSession(sessionKey)
	active.UnlockWithoutUpdate()
	agent := &blockingListAgent{called: make(chan struct{}), release: make(chan struct{})}
	ws.agent = agent
	ws.mu.Unlock()

	attachDone := make(chan error, 1)
	go func() {
		_, err := e.SessionsAttach(sessionKey, "thread-new")
		attachDone <- err
	}()
	<-agent.called

	unbindDone := make(chan error, 1)
	go func() {
		_, err := e.WorkspaceUnbind(sessionKey, worktree, "thread-exact")
		unbindDone <- err
	}()
	select {
	case err := <-unbindDone:
		t.Fatalf("unbind passed lifecycle lock before attach persisted: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(agent.release)
	if err := <-attachDone; err != nil {
		t.Fatalf("attach failed: %v", err)
	}
	if err := <-unbindDone; err == nil {
		t.Fatal("stale guarded unbind succeeded after attach changed native session")
	}
	if e.workspaceBindings.LookupExact("project:project", "feishu:chat") == nil {
		t.Fatal("stale unbind removed the route")
	}
}

func TestSessionsAttach_AcquiresLifecycleLockBeforeResolvingRoute(t *testing.T) {
	e, _, worktree, sessionKey, _ := setupBusyCloseout(t)
	ws := e.workspacePool.Get(worktree)
	ws.mu.Lock()
	ws.sessions.ActiveSession(sessionKey).UnlockWithoutUpdate()
	agent := &blockingListAgent{called: make(chan struct{}), release: make(chan struct{})}
	ws.agent = agent
	ws.mu.Unlock()

	e.lifecycleControlMu.Lock()
	attachDone := make(chan error, 1)
	go func() {
		_, err := e.SessionsAttach(sessionKey, "thread-new")
		attachDone <- err
	}()
	// Give the attach goroutine an opportunity to reach lifecycleControlMu.
	time.Sleep(20 * time.Millisecond)
	changed, err := e.workspaceBindings.UnbindCAS("project:project", "feishu:chat", worktree)
	if err != nil || !changed {
		t.Fatalf("remove route while lifecycle lock is held: changed=%v err=%v", changed, err)
	}
	e.lifecycleControlMu.Unlock()

	select {
	case err := <-attachDone:
		var lifecycleErr *LifecycleError
		if !errors.As(err, &lifecycleErr) || lifecycleErr.Code != "not_found" {
			t.Fatalf("attach error=%v, want not_found after locked revalidation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("attach did not return after lifecycle lock release")
	}
	select {
	case <-agent.called:
		t.Fatal("attach listed native sessions using a route removed before locked validation")
	default:
	}
}

func TestLifecycleWorkspaceUnbind_BusyMismatchedCASConflictsAndRetainsRoute(t *testing.T) {
	e, server, worktree, sessionKey, live := setupBusyCloseout(t)
	w := postCloseout(t, server, map[string]string{"project": "project", "session": sessionKey, "worktree": worktree, "expected_agent_session_id": "another-thread"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["schema_version"] != float64(1) || envelope["ok"] != false || envelope["result"] != nil {
		t.Fatalf("inconsistent error envelope: %s", w.Body.String())
	}
	errorValue, ok := envelope["error"].(map[string]any)
	if !ok || errorValue["code"] != "state_conflict" {
		t.Fatalf("missing nested lifecycle error: %s", w.Body.String())
	}
	if e.workspaceBindings.LookupExact("project:project", "feishu:chat") == nil {
		t.Fatal("mismatched closeout removed route")
	}
	if live.closeCalls != 0 {
		t.Fatal("mismatched closeout interrupted active process")
	}
}

var _ AgentSession = (*closeoutAgentSession)(nil)
var _ = context.Background
