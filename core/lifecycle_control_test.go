package core

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type closeoutAgentSession struct {
	id         string
	closeCalls int
}

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
		OK     bool `json:"ok"`
		Result struct {
			CloseoutGuard CloseoutGuard `json:"closeout_guard"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || !envelope.Result.CloseoutGuard.Verified || !envelope.Result.CloseoutGuard.ActiveTurnPreserved {
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

func TestLifecycleWorkspaceUnbind_BusyMismatchedCASConflictsAndRetainsRoute(t *testing.T) {
	e, server, worktree, sessionKey, live := setupBusyCloseout(t)
	w := postCloseout(t, server, map[string]string{"project": "project", "session": sessionKey, "worktree": worktree, "expected_agent_session_id": "another-thread"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
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
