package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LifecycleError is a stable error returned by the local lifecycle-control API.
type LifecycleError struct {
	Code    string
	Message string
}

func (e *LifecycleError) Error() string { return e.Message }

func lifecycleError(code, format string, args ...any) error {
	return &LifecycleError{Code: code, Message: fmt.Sprintf(format, args...)}
}

type WorkspaceRouteState struct {
	Scope     string `json:"scope"`
	Worktree  string `json:"worktree"`
	Available bool   `json:"available"`
}

type WorkspaceStatusResult struct {
	Project        string               `json:"project"`
	Session        string               `json:"session"`
	ChannelKey     string               `json:"channel_key"`
	ProjectRoute   *WorkspaceRouteState `json:"project_route,omitempty"`
	SharedRoute    *WorkspaceRouteState `json:"shared_route,omitempty"`
	EffectiveRoute *WorkspaceRouteState `json:"effective_route,omitempty"`
}

type CloseoutGuard struct {
	ExpectedAgentSessionID string `json:"expected_agent_session_id"`
	ActiveAgentSessionID   string `json:"active_agent_session_id,omitempty"`
	LiveAgentSessionID     string `json:"live_agent_session_id,omitempty"`
	Busy                   bool   `json:"busy"`
	Verified               bool   `json:"verified"`
	ActiveTurnPreserved    bool   `json:"active_turn_preserved"`
}

type WorkspaceMutationResult struct {
	Project       string         `json:"project"`
	Session       string         `json:"session"`
	Worktree      string         `json:"worktree"`
	Changed       bool           `json:"changed"`
	Status        string         `json:"status"`
	CloseoutGuard *CloseoutGuard `json:"closeout_guard,omitempty"`
}

type SessionControlResult struct {
	Project        string `json:"project"`
	Session        string `json:"session"`
	Worktree       string `json:"worktree"`
	InternalID     string `json:"internal_session_id,omitempty"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	Busy           bool   `json:"busy"`
	Live           bool   `json:"live"`
	Changed        bool   `json:"changed,omitempty"`
}

func (e *Engine) validateLifecycleSession(sessionKey string) (string, error) {
	if !e.multiWorkspace || e.workspaceBindings == nil {
		return "", lifecycleError("state_conflict", "project %q does not use multi-workspace routing", e.name)
	}
	platformName := extractPlatformName(sessionKey)
	found := false
	for _, p := range e.platforms {
		if strings.EqualFold(p.Name(), platformName) {
			found = true
			break
		}
	}
	if !found {
		return "", lifecycleError("not_found", "platform %q is not configured for project %q", platformName, e.name)
	}
	channelKey := extractWorkspaceChannelKey(sessionKey)
	if channelKey == "" {
		return "", lifecycleError("invalid_argument", "session %q has no channel identifier", sessionKey)
	}
	return channelKey, nil
}

func routeState(scope string, binding *WorkspaceBinding) *WorkspaceRouteState {
	if binding == nil {
		return nil
	}
	path := normalizeWorkspacePath(binding.Workspace)
	info, err := os.Stat(path)
	return &WorkspaceRouteState{Scope: scope, Worktree: path, Available: err == nil && info.IsDir()}
}

func (e *Engine) WorkspaceStatus(sessionKey string) (*WorkspaceStatusResult, error) {
	channelKey, err := e.validateLifecycleSession(sessionKey)
	if err != nil {
		return nil, err
	}
	project := routeState("project", e.workspaceBindings.LookupExact("project:"+e.name, channelKey))
	shared := routeState("shared", e.workspaceBindings.LookupExact(sharedWorkspaceBindingsKey, channelKey))
	effective := project
	if effective == nil {
		effective = shared
	}
	return &WorkspaceStatusResult{Project: e.name, Session: sessionKey, ChannelKey: channelKey, ProjectRoute: project, SharedRoute: shared, EffectiveRoute: effective}, nil
}

func canonicalWorktree(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", lifecycleError("invalid_argument", "worktree must be an absolute path")
	}
	path = normalizeWorkspacePath(path)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", lifecycleError("not_found", "worktree %q is not an existing directory", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", lifecycleError("internal_error", "resolve worktree: %v", err)
	}
	return filepath.Clean(resolved), nil
}

func (e *Engine) WorkspaceRoute(sessionKey, worktree string) (*WorkspaceMutationResult, error) {
	channelKey, err := e.validateLifecycleSession(sessionKey)
	if err != nil {
		return nil, err
	}
	worktree, err = canonicalWorktree(worktree)
	if err != nil {
		return nil, err
	}
	e.lifecycleControlMu.Lock()
	defer e.lifecycleControlMu.Unlock()
	if shared := e.workspaceBindings.LookupExact(sharedWorkspaceBindingsKey, channelKey); shared != nil {
		return nil, lifecycleError("state_conflict", "shared route already controls this session")
	}
	if current := e.workspaceBindings.LookupExact("project:"+e.name, channelKey); current != nil {
		currentWorktree, currentErr := canonicalWorktree(current.Workspace)
		if currentErr == nil && currentWorktree == worktree {
			return &WorkspaceMutationResult{Project: e.name, Session: sessionKey, Worktree: worktree, Changed: false, Status: "already_routed"}, nil
		}
	}
	busy, err := e.sessionBusyAt(worktree, sessionKey)
	if err != nil {
		return nil, lifecycleError("internal_error", "open workspace context: %v", err)
	}
	if busy {
		return nil, lifecycleError("state_conflict", "session is busy")
	}
	changed, err := e.workspaceBindings.BindCAS("project:"+e.name, channelKey, "", worktree)
	if err != nil {
		if errors.Is(err, ErrWorkspaceBindingConflict) {
			return nil, lifecycleError("state_conflict", "%v", err)
		}
		return nil, lifecycleError("internal_error", "%v", err)
	}
	status := "routed"
	if !changed {
		status = "already_routed"
	}
	return &WorkspaceMutationResult{Project: e.name, Session: sessionKey, Worktree: worktree, Changed: changed, Status: status}, nil
}

func (e *Engine) sessionBusyAt(worktree, sessionKey string) (bool, error) {
	_, sessions, err := e.getOrCreateWorkspaceAgent(worktree)
	if err != nil {
		return false, err
	}
	s := sessions.ActiveSession(sessionKey)
	return s != nil && s.Busy(), nil
}

// WorkspaceUnbind implements guarded task closeout. A busy session may only
// unbind when all three native-session views match the caller's exact ID.
func (e *Engine) WorkspaceUnbind(sessionKey, worktree, expectedAgentSessionID string) (*WorkspaceMutationResult, error) {
	channelKey, err := e.validateLifecycleSession(sessionKey)
	if err != nil {
		return nil, err
	}
	worktree, err = canonicalWorktree(worktree)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(expectedAgentSessionID) == "" {
		return nil, lifecycleError("invalid_argument", "expected_agent_session_id is required")
	}
	e.lifecycleControlMu.Lock()
	defer e.lifecycleControlMu.Unlock()
	if shared := e.workspaceBindings.LookupExact(sharedWorkspaceBindingsKey, channelKey); shared != nil {
		return nil, lifecycleError("state_conflict", "shared routes cannot be removed by project closeout")
	}
	bound := e.workspaceBindings.LookupExact("project:"+e.name, channelKey)
	if bound == nil {
		return &WorkspaceMutationResult{Project: e.name, Session: sessionKey, Worktree: worktree, Status: "already_unbound", CloseoutGuard: &CloseoutGuard{ExpectedAgentSessionID: expectedAgentSessionID}}, nil
	}
	boundWorktree, boundErr := canonicalWorktree(bound.Workspace)
	if boundErr != nil || boundWorktree != worktree {
		return nil, lifecycleError("state_conflict", "route points at %q, not expected worktree", bound.Workspace)
	}
	_, sessions, err := e.getOrCreateWorkspaceAgent(worktree)
	if err != nil {
		return nil, lifecycleError("internal_error", "open workspace context: %v", err)
	}
	session := sessions.ActiveSession(sessionKey)
	guard := &CloseoutGuard{ExpectedAgentSessionID: expectedAgentSessionID}
	if session != nil {
		guard.ActiveAgentSessionID = session.GetAgentSessionID()
		guard.Busy = session.Busy()
	}
	if guard.ActiveAgentSessionID != expectedAgentSessionID {
		return nil, lifecycleError("state_conflict", "active native session does not match expected ID")
	}
	interactiveKey := worktree + ":" + sessionKey
	e.interactiveMu.Lock()
	state := e.interactiveStates[interactiveKey]
	if state != nil {
		state.mu.Lock()
		if state.currentSessionKey != sessionKey {
			state.mu.Unlock()
			e.interactiveMu.Unlock()
			return nil, lifecycleError("state_conflict", "live state belongs to another session")
		}
		if state.agentSession != nil && state.agentSession.Alive() {
			guard.LiveAgentSessionID = state.agentSession.CurrentSessionID()
		}
		state.mu.Unlock()
	}
	e.interactiveMu.Unlock()
	if guard.Busy {
		if state == nil || guard.LiveAgentSessionID != expectedAgentSessionID {
			return nil, lifecycleError("state_conflict", "busy closeout requires the exact live native session")
		}
		if e.trackStore != nil {
			p := e.platformForName(extractPlatformName(sessionKey))
			if destination, destinationErr := mirrorDestinationKey(p, sessionKey); destinationErr == nil {
				if mirror := e.trackStore.binding(destination); mirror != nil && mirror.ThreadID != "" && mirror.ThreadID != expectedAgentSessionID {
					return nil, lifecycleError("state_conflict", "conversation mirror points at another native session")
				}
			}
		}
		guard.Verified = true
		guard.ActiveTurnPreserved = true
	}
	changed, err := e.workspaceBindings.UnbindCAS("project:"+e.name, channelKey, bound.Workspace)
	if err != nil {
		return nil, lifecycleError("internal_error", "%v", err)
	}
	status := "unbound"
	if !changed {
		status = "already_unbound"
	}
	return &WorkspaceMutationResult{Project: e.name, Session: sessionKey, Worktree: worktree, Changed: changed, Status: status, CloseoutGuard: guard}, nil
}

// lifecycleWorkspaceContextLocked resolves a project route while the caller
// holds lifecycleControlMu, keeping route validation and subsequent mutation
// in one critical section.
func (e *Engine) lifecycleWorkspaceContextLocked(sessionKey string) (string, Agent, *SessionManager, error) {
	channelKey, err := e.validateLifecycleSession(sessionKey)
	if err != nil {
		return "", nil, nil, err
	}
	if e.workspaceBindings.LookupExact(sharedWorkspaceBindingsKey, channelKey) != nil {
		return "", nil, nil, lifecycleError("state_conflict", "session is controlled by a shared route")
	}
	binding := e.workspaceBindings.LookupExact("project:"+e.name, channelKey)
	if binding == nil {
		return "", nil, nil, lifecycleError("not_found", "project route not found")
	}
	worktree, err := canonicalWorktree(binding.Workspace)
	if err != nil {
		return "", nil, nil, err
	}
	agent, sessions, err := e.getOrCreateWorkspaceAgent(worktree)
	if err != nil {
		return "", nil, nil, lifecycleError("internal_error", "open workspace context: %v", err)
	}
	return worktree, agent, sessions, nil
}

func (e *Engine) SessionsStatus(sessionKey string) (*SessionControlResult, error) {
	e.lifecycleControlMu.Lock()
	defer e.lifecycleControlMu.Unlock()
	worktree, _, sessions, err := e.lifecycleWorkspaceContextLocked(sessionKey)
	if err != nil {
		return nil, err
	}
	return e.sessionsStatusLocked(sessionKey, worktree, sessions), nil
}

func (e *Engine) sessionsStatusLocked(sessionKey, worktree string, sessions *SessionManager) *SessionControlResult {
	result := &SessionControlResult{Project: e.name, Session: sessionKey, Worktree: worktree}
	if active := sessions.ActiveSession(sessionKey); active != nil {
		result.InternalID = active.ID
		result.AgentSessionID = active.GetAgentSessionID()
		result.Busy = active.Busy()
	}
	key := worktree + ":" + sessionKey
	e.interactiveMu.Lock()
	state := e.interactiveStates[key]
	if state != nil {
		state.mu.Lock()
		result.Live = state.agentSession != nil && state.agentSession.Alive()
		state.mu.Unlock()
	}
	e.interactiveMu.Unlock()
	return result
}

func (e *Engine) SessionsAttach(sessionKey, agentSessionID string) (*SessionControlResult, error) {
	if strings.TrimSpace(agentSessionID) == "" {
		return nil, lifecycleError("invalid_argument", "agent_session_id is required")
	}
	e.lifecycleControlMu.Lock()
	defer e.lifecycleControlMu.Unlock()
	worktree, agent, sessions, err := e.lifecycleWorkspaceContextLocked(sessionKey)
	if err != nil {
		return nil, err
	}
	active := sessions.ActiveSession(sessionKey)
	if active != nil && active.GetAgentSessionID() == agentSessionID {
		result := e.sessionsStatusLocked(sessionKey, worktree, sessions)
		result.Changed = false
		return result, nil
	}
	if active != nil && active.Busy() {
		return nil, lifecycleError("state_conflict", "session is busy")
	}
	available, err := agent.ListSessions(e.ctx)
	if err != nil {
		return nil, lifecycleError("internal_error", "list native sessions: %v", err)
	}
	var matched *AgentSessionInfo
	for i := range available {
		if available[i].ID == agentSessionID {
			matched = &available[i]
			break
		}
	}
	if matched == nil {
		return nil, lifecycleError("not_found", "native session %q not found in routed worktree", agentSessionID)
	}
	p := e.platformForName(extractPlatformName(sessionKey))
	binding, err := e.rebindConversationMirror(p, sessionKey, matched.ID, agent)
	if err != nil {
		return nil, lifecycleError("internal_error", "persist conversation mirror: %v", err)
	}
	e.cleanupInteractiveState(worktree + ":" + sessionKey)
	session, err := sessions.SwitchToAgentSessionChecked(sessionKey, matched.ID, agent.Name(), matched.Summary)
	if err != nil {
		return nil, lifecycleError("internal_error", "persist session attachment: %v", err)
	}
	e.startConversationMirror(agent, sessions, p, binding)
	return &SessionControlResult{Project: e.name, Session: sessionKey, Worktree: worktree, InternalID: session.ID, AgentSessionID: matched.ID, Changed: true}, nil
}
