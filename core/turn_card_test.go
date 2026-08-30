package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type nativeTurnCardPlatform struct {
	stubCompactProgressPlatform
	cardMessageID string
}

func newNativeTurnCardPlatform() *nativeTurnCardPlatform {
	return &nativeTurnCardPlatform{
		stubCompactProgressPlatform: stubCompactProgressPlatform{
			stubPlatformEngine: stubPlatformEngine{n: "feishu"},
			style:              progressStyleCard,
			supportPayload:     true,
		},
		cardMessageID: "om-native-card-1",
	}
}

func (p *nativeTurnCardPlatform) PreviewMessageID(any) (string, error) {
	if p.cardMessageID == "" {
		return "", fmt.Errorf("missing native card message id")
	}
	return p.cardMessageID, nil
}

func (*nativeTurnCardPlatform) SupportsExactTurnCards() bool { return true }

type nativeTurnCardAgent struct {
	*mirrorTestAgent
	session      AgentSession
	interruptErr error
}

type steerOnlyNativeTurnAgent struct{ stubAgent }

func (*steerOnlyNativeTurnAgent) GetConversation(context.Context, string, int) (*ConversationSnapshot, error) {
	return &ConversationSnapshot{SessionID: "thread-1", Turns: []ConversationTurn{{ID: "turn-1", Status: ConversationTurnInProgress}}}, nil
}

func (*steerOnlyNativeTurnAgent) SteerConversationTurn(context.Context, string, string, string, string, []ImageAttachment, []FileAttachment) error {
	return nil
}

type nativeTurnSignalSession struct {
	*controllableAgentSession
	sent chan struct{}
}

func newNativeTurnSignalSession(id string) *nativeTurnSignalSession {
	return &nativeTurnSignalSession{controllableAgentSession: newControllableSession(id), sent: make(chan struct{}, 1)}
}

func (s *nativeTurnSignalSession) Send(string, string, []ImageAttachment, []FileAttachment) error {
	s.sent <- struct{}{}
	return nil
}

func (a *nativeTurnCardAgent) StartSession(context.Context, string) (AgentSession, error) {
	return a.session, nil
}

func (a *nativeTurnCardAgent) InterruptConversationTurn(_ context.Context, sessionID, turnID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.interruptErr != nil {
		return a.interruptErr
	}
	a.interrupts = append(a.interrupts, [2]string{sessionID, turnID})
	return nil
}

func waitTurnCardTest(t *testing.T, label string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

type activeNativeTurnCardFixture struct {
	e       *Engine
	agent   *nativeTurnCardAgent
	p       *nativeTurnCardPlatform
	state   *interactiveState
	session *Session
	runtime *controllableAgentSession
	key     string
	action  string
	done    chan struct{}
}

func startActiveNativeTurnCard(t *testing.T) *activeNativeTurnCardFixture {
	t.Helper()
	running := ConversationTurn{ID: "turn-1", Status: ConversationTurnInProgress}
	base := newMirrorTestAgent(&ConversationSnapshot{SessionID: "thread-1", Turns: []ConversationTurn{running}})
	runtimeSession := newControllableSession("thread-1")
	agent := &nativeTurnCardAgent{mirrorTestAgent: base, session: runtimeSession}
	p := newNativeTurnCardPlatform()
	e := NewEngine("test", agent, []Platform{p}, t.TempDir()+"/sessions.json", LangChinese)
	key := "feishu:chat:user"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	if !session.TryLock() {
		t.Fatal("TryLock() = false")
	}
	state := &interactiveState{
		agentSession:      runtimeSession,
		platform:          p,
		replyCtx:          "ctx",
		agent:             agent,
		currentSessionKey: key,
	}
	e.interactiveStates[key] = state
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "prompt-message", time.Now(), nil, nil, "ctx")
		close(done)
	}()
	runtimeSession.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1", ClientUserMessageID: "prompt-message"}
	runtimeSession.events <- Event{Type: EventThinking, Content: "正在检查"}

	var action string
	waitTurnCardTest(t, "native card controls", func() bool {
		edits := p.getPreviewEdits()
		for index := len(edits) - 1; index >= 0; index-- {
			payload, ok := ParseProgressCardPayload(edits[index])
			if ok && payload.State == ProgressCardStateRunning && payload.Hint != "" && len(payload.Buttons) == 1 {
				action = payload.Buttons[0].Value
				return true
			}
		}
		return false
	})
	return &activeNativeTurnCardFixture{
		e: e, agent: agent, p: p, state: state, session: session, runtime: runtimeSession,
		key: key, action: action, done: done,
	}
}

func (f *activeNativeTurnCardFixture) finish(t *testing.T, status string) {
	t.Helper()
	f.runtime.events <- Event{
		Type: EventResult, ThreadID: "thread-1", TurnID: "turn-1", Content: "任务结束", Done: true,
		Metadata: map[string]any{"turn_status": status},
	}
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("native turn event loop did not finish")
	}
	f.session.Unlock()
}

func latestNativeTurnPayload(t *testing.T, p *nativeTurnCardPlatform) *ProgressCardPayload {
	t.Helper()
	edits := p.getPreviewEdits()
	if len(edits) == 0 {
		t.Fatal("native card has no updates")
	}
	payload, ok := ParseProgressCardPayload(edits[len(edits)-1])
	if !ok {
		t.Fatalf("latest native card payload did not parse: %q", edits[len(edits)-1])
	}
	return payload
}

func TestNativeTurnCard_HealthMonitorAndTrackReuseExistingCard(t *testing.T) {
	f := startActiveNativeTurnCard(t)
	f.e.SetAdminFrom("admin")
	waitTurnCardTest(t, "verified native card health", func() bool {
		payload := latestNativeTurnPayload(t, f.p)
		return payload.Health == ProgressCardHealthVerified && payload.LastVerifiedAt > 0
	})
	startsBefore := len(f.p.getPreviewStarts())
	readsBefore := f.agent.readCount()
	f.e.handleCommand(f.p, &Message{
		SessionKey: f.key, Platform: f.p.Name(), MessageID: "track-native",
		UserID: "admin", Content: "/track", ReplyCtx: "track-ctx",
	}, "/track")
	if startsAfter := len(f.p.getPreviewStarts()); startsAfter != startsBefore {
		t.Fatalf("/track created a duplicate native card: before=%d after=%d", startsBefore, startsAfter)
	}
	if f.agent.readCount() <= readsBefore {
		t.Fatal("/track did not perform an authoritative status check")
	}
	if got := strings.Join(f.p.getSent(), "\n"); !strings.Contains(got, "已同步现有原生任务卡片") {
		t.Fatalf("/track native-card reply = %q", got)
	}
	f.finish(t, "completed")
}

func TestNativeTurnCard_HealthMonitorShowsFailureAndRecovery(t *testing.T) {
	f := startActiveNativeTurnCard(t)
	token := strings.TrimPrefix(f.action, turnCardInterruptActionPrefix)
	monitor := f.e.nativeTurnCardMonitor(token)
	if monitor == nil {
		t.Fatal("native turn monitor not registered")
	}

	f.agent.mu.Lock()
	f.agent.readErr = errors.New("injected authoritative read failure")
	f.agent.mu.Unlock()
	health, _, err := f.e.checkNativeTurnCardMonitor(f.e.ctx, monitor, nil)
	if err == nil || health != ProgressCardHealthReconnecting {
		t.Fatalf("first failed check = health %q error %v", health, err)
	}
	payload := latestNativeTurnPayload(t, f.p)
	if payload.Health != ProgressCardHealthReconnecting || len(payload.Buttons) == 0 {
		t.Fatalf("reconnecting payload = health %q buttons %#v", payload.Health, payload.Buttons)
	}

	monitor.checkMu.Lock()
	monitor.failedAt = time.Now().Add(-cardHealthUnknownAfter)
	monitor.checkMu.Unlock()
	health, _, err = f.e.checkNativeTurnCardMonitor(f.e.ctx, monitor, nil)
	if err == nil || health != ProgressCardHealthUnknown {
		t.Fatalf("expired failed check = health %q error %v", health, err)
	}
	payload = latestNativeTurnPayload(t, f.p)
	if payload.Health != ProgressCardHealthUnknown {
		t.Fatalf("unknown payload health = %q", payload.Health)
	}

	f.agent.mu.Lock()
	f.agent.readErr = nil
	f.agent.mu.Unlock()
	health, status, err := f.e.checkNativeTurnCardMonitor(f.e.ctx, monitor, nil)
	if err != nil || health != ProgressCardHealthVerified || status != ConversationTurnInProgress {
		t.Fatalf("recovered check = health %q status %q error %v", health, status, err)
	}
	payload = latestNativeTurnPayload(t, f.p)
	if payload.Health != ProgressCardHealthVerified || len(payload.Buttons) != 1 {
		t.Fatalf("recovered payload = health %q buttons %#v", payload.Health, payload.Buttons)
	}
	f.finish(t, "completed")
}

func TestNativeTurnCard_HealthMonitorSynchronizesTerminalState(t *testing.T) {
	f := startActiveNativeTurnCard(t)
	token := strings.TrimPrefix(f.action, turnCardInterruptActionPrefix)
	monitor := f.e.nativeTurnCardMonitor(token)
	if monitor == nil {
		t.Fatal("native turn monitor not registered")
	}

	f.agent.setSnapshot(&ConversationSnapshot{
		SessionID: "thread-1",
		Turns:     []ConversationTurn{{ID: "turn-1", Status: ConversationTurnCompleted}},
	})
	health, status, err := f.e.checkNativeTurnCardMonitor(f.e.ctx, monitor, nil)
	if err != nil || health != ProgressCardHealthVerified || status != ConversationTurnCompleted {
		t.Fatalf("terminal check = health %q status %q error %v", health, status, err)
	}
	payload := latestNativeTurnPayload(t, f.p)
	if payload.State != ProgressCardStateCompleted {
		t.Fatalf("terminal payload state = %q", payload.State)
	}
	stored := f.e.turnCards.byToken(token)
	if stored == nil || !stored.Terminal || stored.Status != string(ConversationTurnCompleted) {
		t.Fatalf("terminal stored identity = %#v", stored)
	}
	if got := f.e.nativeTurnCardMonitor(token); got != nil {
		t.Fatalf("terminal monitor still registered: %#v", got)
	}

	// Let the foreground event loop unwind as it normally would; the terminal
	// update is idempotent when the delayed result event finally arrives.
	f.finish(t, "completed")
}

func TestNativeTurnCard_TrackReplacesStaleRunningCard(t *testing.T) {
	f := startActiveNativeTurnCard(t)
	f.e.SetAdminFrom("admin")
	token := strings.TrimPrefix(f.action, turnCardInterruptActionPrefix)
	f.e.stopNativeTurnCardMonitor(token)
	if err := f.e.turnCards.markTerminal(token, "stale"); err != nil {
		t.Fatalf("markTerminal(stale) error = %v", err)
	}
	startsBefore := len(f.p.getPreviewStarts())
	f.e.ReceiveMessage(f.p, &Message{
		SessionKey: f.key, Platform: f.p.Name(), MessageID: "track-stale-native",
		UserID: "admin", Content: "/track", ReplyCtx: "track-ctx",
	})
	if startsAfter := len(f.p.getPreviewStarts()); startsAfter != startsBefore+1 {
		t.Fatalf("/track replacement cards: before=%d after=%d", startsBefore, startsAfter)
	}
	f.e.trackMu.Lock()
	tracker := f.e.trackers[f.key]
	f.e.trackMu.Unlock()
	if tracker == nil || tracker.sessionID != "thread-1" || tracker.turnID != "turn-1" {
		t.Fatalf("stale native /track tracker = %#v", tracker)
	}
	if got := strings.Join(f.p.getSent(), "\n"); strings.Contains(got, "已同步") {
		t.Fatalf("stale native /track falsely claimed synchronization: %q", got)
	}
	f.e.cancelConversationTracker(f.key)
	f.finish(t, "completed")
}

func TestNativeTurnCard_ReplySteersAndInterruptIsSilentAndExact(t *testing.T) {
	f := startActiveNativeTurnCard(t)
	// The same card message seen from another logical session is recognized but
	// rejected, so a cross-member/cross-session reply cannot start a new turn.
	f.e.ReceiveMessage(f.p, &Message{
		SessionKey: "feishu:chat:other-user", Platform: f.p.Name(), MessageID: "foreign-reply", ReferencedMessageID: f.p.cardMessageID,
		UserID: "other", Content: "不要进入另一个会话", ReplyCtx: "foreign-ctx",
	})
	f.agent.mu.Lock()
	foreignSteers := len(f.agent.steers)
	f.agent.mu.Unlock()
	if foreignSteers != 0 || !strings.Contains(strings.Join(f.p.getSent(), "\n"), "输入未发送") {
		t.Fatalf("foreign-session reply was not fail-closed: steers=%d replies=%q", foreignSteers, strings.Join(f.p.getSent(), "\n"))
	}
	f.p.clearSent()

	accepted := 0
	f.e.ReceiveMessage(f.p, &Message{
		SessionKey: f.key, Platform: f.p.Name(), MessageID: "steer-message", ReferencedMessageID: f.p.cardMessageID,
		UserID: "member", Content: "再检查一下测试", ReplyCtx: "steer-ctx", OnAccepted: func() { accepted++ },
	})
	f.agent.mu.Lock()
	steers := append([][4]string(nil), f.agent.steers...)
	f.agent.mu.Unlock()
	if len(steers) != 1 || steers[0] != [4]string{"thread-1", "turn-1", "再检查一下测试", "steer-message"} {
		t.Fatalf("exact steers = %#v", steers)
	}
	if accepted != 1 || !strings.Contains(strings.Join(f.p.getSent(), "\n"), "已将输入追加") {
		t.Fatalf("steer acceptance = accepted %d replies %q", accepted, strings.Join(f.p.getSent(), "\n"))
	}

	f.p.clearSent()
	f.e.ReceiveMessage(f.p, &Message{
		SessionKey: f.key, Platform: f.p.Name(), ReferencedMessageID: f.p.cardMessageID,
		UserID: "member", Content: f.action, ReplyCtx: "stop-ctx", IsCardAction: true,
	})
	f.agent.mu.Lock()
	interrupts := append([][2]string(nil), f.agent.interrupts...)
	f.agent.mu.Unlock()
	if len(interrupts) != 1 || interrupts[0] != [2]string{"thread-1", "turn-1"} {
		t.Fatalf("exact interrupts = %#v", interrupts)
	}
	if got := f.p.getSent(); len(got) != 0 {
		t.Fatalf("successful interrupt sent an independent message: %#v", got)
	}

	// A concurrent/repeated click cannot invoke the controller twice.
	f.e.ReceiveMessage(f.p, &Message{
		SessionKey: f.key, Platform: f.p.Name(), ReferencedMessageID: f.p.cardMessageID,
		UserID: "member", Content: f.action, ReplyCtx: "stop-ctx-2", IsCardAction: true,
	})
	f.agent.mu.Lock()
	interruptCount := len(f.agent.interrupts)
	f.agent.mu.Unlock()
	if interruptCount != 1 {
		t.Fatalf("repeated click invoked %d interrupts, want 1", interruptCount)
	}
	if got := strings.Join(f.p.getSent(), "\n"); !strings.Contains(got, "已失效") {
		t.Fatalf("repeated click response = %q, want stale", got)
	}

	f.finish(t, "interrupted")
	edits := f.p.getPreviewEdits()
	terminal, ok := ParseProgressCardPayload(edits[len(edits)-1])
	if !ok {
		t.Fatalf("terminal progress payload did not parse: %q", edits[len(edits)-1])
	}
	if terminal.State != ProgressCardStateInterrupted || terminal.Hint != "" || len(terminal.Buttons) != 0 {
		t.Fatalf("terminal card = state %q hint %q buttons %#v", terminal.State, terminal.Hint, terminal.Buttons)
	}
	token := strings.TrimPrefix(f.action, turnCardInterruptActionPrefix)
	stored := f.e.turnCards.byToken(token)
	if stored == nil || !stored.Terminal || stored.Status != string(ConversationTurnInterrupted) {
		t.Fatalf("terminal stored identity = %#v", stored)
	}

	// Replies to the terminal card remain fail-closed instead of becoming a new turn.
	f.p.clearSent()
	f.e.ReceiveMessage(f.p, &Message{
		SessionKey: f.key, Platform: f.p.Name(), MessageID: "late-reply", ReferencedMessageID: f.p.cardMessageID,
		UserID: "member", Content: "这条不能启动新任务", ReplyCtx: "late-ctx",
	})
	f.agent.mu.Lock()
	steerCount := len(f.agent.steers)
	f.agent.mu.Unlock()
	if steerCount != 1 || !strings.Contains(strings.Join(f.p.getSent(), "\n"), "输入未发送") {
		t.Fatalf("late reply was not fail-closed: steers=%d replies=%q", steerCount, strings.Join(f.p.getSent(), "\n"))
	}
}

func TestNativeTurnCard_ControlsWaitForAuthoritativeTurnIdentity(t *testing.T) {
	running := ConversationTurn{ID: "turn-1", Status: ConversationTurnInProgress}
	runtimeSession := newControllableSession("thread-1")
	agent := &nativeTurnCardAgent{
		mirrorTestAgent: newMirrorTestAgent(&ConversationSnapshot{SessionID: "thread-1", Turns: []ConversationTurn{running}}),
		session:         runtimeSession,
	}
	p := newNativeTurnCardPlatform()
	e := NewEngine("test", agent, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
	key := "feishu:chat:user"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	if !session.TryLock() {
		t.Fatal("TryLock() = false")
	}
	state := &interactiveState{
		agentSession: runtimeSession, platform: p, replyCtx: "ctx", agent: agent, currentSessionKey: key,
	}
	e.interactiveStates[key] = state
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "prompt-message", time.Now(), nil, nil, "ctx")
		close(done)
	}()

	// Progress can arrive before the backend identity event. The initial card
	// must remain informational until both authoritative IDs are available.
	runtimeSession.events <- Event{Type: EventThinking, Content: "inspect"}
	waitTurnCardTest(t, "uncontrolled initial progress card", func() bool { return len(p.getPreviewStarts()) == 1 })
	initial, ok := ParseProgressCardPayload(p.getPreviewStarts()[0])
	if !ok || initial.Hint != "" || len(initial.Buttons) != 0 {
		t.Fatalf("pre-identity progress controls = %#v", initial)
	}

	runtimeSession.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: "turn-1"}
	waitTurnCardTest(t, "post-identity exact controls", func() bool {
		for _, edit := range p.getPreviewEdits() {
			payload, parsed := ParseProgressCardPayload(edit)
			if parsed && payload.Hint != "" && len(payload.Buttons) == 1 {
				return true
			}
		}
		return false
	})

	runtimeSession.events <- Event{Type: EventResult, Done: true, Content: "done", Metadata: map[string]any{"turn_status": "completed"}}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("native turn event loop did not finish")
	}
	session.Unlock()
}

func TestNativeTurnCard_FailedInterruptCanBeRetried(t *testing.T) {
	f := startActiveNativeTurnCard(t)
	f.agent.mu.Lock()
	f.agent.interruptErr = fmt.Errorf("temporary control failure")
	f.agent.mu.Unlock()

	f.e.ReceiveMessage(f.p, &Message{
		SessionKey: f.key, Platform: f.p.Name(), ReferencedMessageID: f.p.cardMessageID,
		UserID: "member", Content: f.action, ReplyCtx: "stop-ctx", IsCardAction: true,
	})
	if got := strings.Join(f.p.getSent(), "\n"); !strings.Contains(got, "temporary control failure") {
		t.Fatalf("failed interrupt response = %q", got)
	}

	f.agent.mu.Lock()
	f.agent.interruptErr = nil
	f.agent.mu.Unlock()
	f.p.clearSent()
	f.e.ReceiveMessage(f.p, &Message{
		SessionKey: f.key, Platform: f.p.Name(), ReferencedMessageID: f.p.cardMessageID,
		UserID: "member", Content: f.action, ReplyCtx: "retry-ctx", IsCardAction: true,
	})
	f.agent.mu.Lock()
	interrupts := append([][2]string(nil), f.agent.interrupts...)
	f.agent.mu.Unlock()
	if len(interrupts) != 1 || interrupts[0] != [2]string{"thread-1", "turn-1"} {
		t.Fatalf("retried exact interrupts = %#v", interrupts)
	}
	if sent := f.p.getSent(); len(sent) != 0 {
		t.Fatalf("successful retry sent an independent message: %#v", sent)
	}
	f.finish(t, "interrupted")
}

func TestNativeTurnCard_RestartTombstoneRejectsReply(t *testing.T) {
	path := t.TempDir() + "/sessions.json"
	first := newTurnCardStore(turnCardStatePath(path))
	card := testTurnCardState("restart-token")
	card.SessionKey = "feishu:chat:user"
	card.InteractiveKey = card.SessionKey
	if err := first.register(card); err != nil {
		t.Fatalf("register() error = %v", err)
	}

	agent := newMirrorTestAgent(&ConversationSnapshot{SessionID: card.ThreadID, Turns: []ConversationTurn{{ID: "new-turn", Status: ConversationTurnInProgress}}})
	p := newNativeTurnCardPlatform()
	e := NewEngine("test", agent, []Platform{p}, path, LangEnglish)
	msg := &Message{
		SessionKey: card.SessionKey, Platform: p.Name(), MessageID: "late-reply",
		ReferencedMessageID: card.CardMessageID, Content: "do not start a new turn", ReplyCtx: "ctx",
	}
	if !e.handleTurnCardReply(p, msg, msg.Content, agent, e.sessions, card.InteractiveKey) {
		t.Fatal("restart tombstone did not recognize the old native card")
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "not sent") {
		t.Fatalf("restart tombstone response = %q", got)
	}
}

func TestNativeTurnCard_OldGenerationCannotInterruptNewTurn(t *testing.T) {
	f := startActiveNativeTurnCard(t)
	f.state.mu.Lock()
	f.state.currentThreadID = "thread-1"
	f.state.currentTurnID = "turn-2"
	f.state.currentTurnGeneration++
	f.state.mu.Unlock()
	f.agent.setSnapshot(&ConversationSnapshot{SessionID: "thread-1", Turns: []ConversationTurn{{ID: "turn-2", Status: ConversationTurnInProgress}}})

	f.e.ReceiveMessage(f.p, &Message{
		SessionKey: f.key, Platform: f.p.Name(), ReferencedMessageID: f.p.cardMessageID,
		UserID: "member", Content: f.action, ReplyCtx: "ctx", IsCardAction: true,
	})
	f.agent.mu.Lock()
	interruptCount := len(f.agent.interrupts)
	f.agent.mu.Unlock()
	if interruptCount != 0 {
		t.Fatalf("old generation interrupted a newer turn: %#v", f.agent.interrupts)
	}
	if got := strings.Join(f.p.getSent(), "\n"); !strings.Contains(got, "已失效") {
		t.Fatalf("old generation response = %q", got)
	}
	f.finish(t, "completed")
}

func TestNativeTurnCard_HidesInterruptButtonWithoutController(t *testing.T) {
	agent := &steerOnlyNativeTurnAgent{}
	p := newNativeTurnCardPlatform()
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	state := &interactiveState{}
	card := e.beginActiveTurnCard(state, p, "feishu:chat:user", "feishu:chat:user", "thread-1", "turn-1", agent)
	if card == nil || card.canInterrupt || !card.canSteer {
		t.Fatalf("turn card capabilities = %#v", card)
	}
	w := newCompactProgressWriter(e.ctx, p, "ctx", agent.Name(), LangEnglish, nil)
	w.SetHandleObserver(func(handle any) { e.activateTurnCard(p, w, card, handle) })
	if !w.AppendEvent(ProgressEntryThinking, "inspect", "", "inspect") {
		t.Fatal("AppendEvent() = false")
	}
	edits := p.getPreviewEdits()
	if len(edits) != 1 {
		t.Fatalf("control edits = %d, want 1", len(edits))
	}
	payload, ok := ParseProgressCardPayload(edits[0])
	if !ok || payload.Hint == "" || len(payload.Buttons) != 0 {
		t.Fatalf("steer-only controls = %#v", payload)
	}
}
