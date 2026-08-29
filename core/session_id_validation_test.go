package core

import (
	"context"
	"testing"
)

// validatingAgent wraps a controllableAgent and adds an opt-in
// SessionIDValidator so we can pin the engine's behavior for issue #599.
type validatingAgent struct {
	controllableAgent
	validateFunc func(ctx context.Context, sessionID string) bool
}

func (a *validatingAgent) ValidateSessionID(ctx context.Context, sessionID string) bool {
	if a.validateFunc == nil {
		return true // default: trust whatever ID the Session has
	}
	return a.validateFunc(ctx, sessionID)
}

var _ SessionIDValidator = (*validatingAgent)(nil)

// TestIssue599_InvalidSessionIDPreservedWithoutFreshStart pins both safety
// requirements: never resume a cross-project ID, and never silently abandon
// the selected conversation by starting a fresh thread.
func TestIssue599_InvalidSessionIDPreservedWithoutFreshStart(t *testing.T) {
	startCalls := 0
	sess := newControllableSession("fresh-id")
	agent := &validatingAgent{
		controllableAgent: controllableAgent{nextSession: sess},
		validateFunc: func(_ context.Context, sessionID string) bool {
			// Reject whatever ID the Session carries — simulate a
			// cross-project leak.
			return false
		},
	}
	agent.startSessionFn = func(_ context.Context, sessionID string) (AgentSession, error) {
		startCalls++
		return sess, nil
	}

	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	key := "test:user1"

	// Simulate a stored cross-project session ID.
	s := &Session{AgentSessionID: "leaked-id-from-other-project"}

	state := e.getOrCreateInteractiveStateWith(key, p, "ctx", s, e.sessions, nil, "")

	if startCalls != 0 {
		t.Errorf("StartSession calls = %d, want 0 for rejected session ID", startCalls)
	}
	if state.agentSession != nil || state.startError == nil {
		t.Fatalf("state = %#v, want visible startup failure without agent session", state)
	}
	if got := s.GetAgentSessionID(); got != "leaked-id-from-other-project" {
		t.Errorf("AgentSessionID = %q, want rejected selection preserved", got)
	}
}

// TestIssue599_ValidSessionIDPreserved is the negative case: when the
// agent says the ID is valid, the engine must pass it through to
// StartSession so the resume actually resumes.
func TestIssue599_ValidSessionIDPreserved(t *testing.T) {
	var startedWith string
	sess := newControllableSession("resumed-id")
	agent := &validatingAgent{
		controllableAgent: controllableAgent{nextSession: sess},
		validateFunc: func(_ context.Context, _ string) bool {
			return true
		},
	}
	agent.startSessionFn = func(_ context.Context, sessionID string) (AgentSession, error) {
		startedWith = sessionID
		return sess, nil
	}

	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	key := "test:user1"

	s := &Session{AgentSessionID: "valid-id-abc"}

	e.getOrCreateInteractiveStateWith(key, p, "ctx", s, e.sessions, nil, "")

	if startedWith != "valid-id-abc" {
		t.Errorf("StartSession called with %q, want %q (resume path)", startedWith, "valid-id-abc")
	}
}

// TestIssue599_AgentWithoutValidatorNotBlocked ensures the validation
// gate is opt-in: agents that do not implement SessionIDValidator
// continue to work as before (the existing assumption is that engine
// callers only persist valid IDs).
func TestIssue599_AgentWithoutValidatorNotBlocked(t *testing.T) {
	var startedWith string
	sess := newControllableSession("resumed-id")
	agent := &controllableAgent{nextSession: sess}
	agent.startSessionFn = func(_ context.Context, sessionID string) (AgentSession, error) {
		startedWith = sessionID
		return sess, nil
	}

	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	key := "test:user1"

	s := &Session{AgentSessionID: "any-id"}

	e.getOrCreateInteractiveStateWith(key, p, "ctx", s, e.sessions, nil, "")

	if startedWith != "any-id" {
		t.Errorf("StartSession called with %q, want %q (no validator = pass through)", startedWith, "any-id")
	}
}
