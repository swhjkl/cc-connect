package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type authoritativeConversationAgent struct {
	stubAgent
	mu           sync.Mutex
	snapshots    []*ConversationSnapshot
	err          error
	calls        int
	interruptErr error
	interrupts   [][2]string
}

func (a *authoritativeConversationAgent) InterruptConversationTurn(_ context.Context, sessionID, turnID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.interruptErr != nil {
		return a.interruptErr
	}
	a.interrupts = append(a.interrupts, [2]string{sessionID, turnID})
	return nil
}

func (a *authoritativeConversationAgent) GetConversation(_ context.Context, _ string, _ int) (*ConversationSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	if len(a.snapshots) == 0 {
		return &ConversationSnapshot{}, nil
	}
	index := a.calls - 1
	if index >= len(a.snapshots) {
		index = len(a.snapshots) - 1
	}
	return a.snapshots[index], nil
}

func (a *authoritativeConversationAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type trackPreviewPlatform struct {
	stubPlatformEngine
	starts  chan string
	updates chan string
}

type trackActionPlatform struct {
	*trackPreviewPlatform
	mu      sync.Mutex
	buttons []CardButton
}

type trackHealthPreviewPlatform struct {
	*trackPreviewPlatform
	mu      sync.Mutex
	options []RichCardRenderOptions
}

func newTrackHealthPreviewPlatform() *trackHealthPreviewPlatform {
	return &trackHealthPreviewPlatform{trackPreviewPlatform: newTrackPreviewPlatform()}
}

func (p *trackHealthPreviewPlatform) BuildRichCardWithOptions(options RichCardRenderOptions) string {
	p.mu.Lock()
	p.options = append(p.options, options)
	p.mu.Unlock()
	return fmt.Sprintf("%#v", options)
}

func (p *trackHealthPreviewPlatform) getOptions() []RichCardRenderOptions {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]RichCardRenderOptions(nil), p.options...)
}

func (*trackHealthPreviewPlatform) PreviewMessageID(handle any) (string, error) {
	messageID, ok := handle.(string)
	if !ok || strings.TrimSpace(messageID) == "" {
		return "", fmt.Errorf("invalid preview handle %T", handle)
	}
	return messageID, nil
}

func (p *trackActionPlatform) BuildRichCard(_ CardStatus, _ string, _ []ToolStep, markdown string, _ bool, _ string) string {
	return markdown
}

func (p *trackActionPlatform) BuildRichCardWithActions(_ CardStatus, _ string, _ []ToolStep, markdown string, _ bool, _ string, buttons []CardButton) string {
	p.mu.Lock()
	p.buttons = append([]CardButton(nil), buttons...)
	p.mu.Unlock()
	return markdown
}

type trackChatAgent struct {
	snapshot *ConversationSnapshot
	session  AgentSession

	mu             sync.Mutex
	startSessionID string
}

type mirrorTestAgent struct {
	stubAgent
	mu         sync.Mutex
	snapshot   *ConversationSnapshot
	events     chan Event
	readErr    error
	watchErr   error
	reads      int
	watches    int
	interrupts [][2]string
	steers     [][4]string
}

type mirrorSessionAgent struct {
	*mirrorTestAgent
	listed []AgentSessionInfo
}

type mirrorNoMarkerAgent struct {
	*mirrorTestAgent
}

func (*mirrorNoMarkerAgent) SupportsConversationClientMarker() bool { return false }

type interactiveMirrorAgent struct {
	*mirrorTestAgent
	session AgentSession
}

func (a *interactiveMirrorAgent) StartSession(context.Context, string) (AgentSession, error) {
	return a.session, nil
}

type markerQueueSession struct {
	controllableAgentSession
	sent chan string
}

type planExecutionSend struct {
	prompt string
	mode   string
	id     string
}

type planExecutionSession struct {
	events chan Event
	sends  chan planExecutionSend
}

func newPlanExecutionSession() *planExecutionSession {
	return &planExecutionSession{events: make(chan Event, 8), sends: make(chan planExecutionSend, 4)}
}

func (s *planExecutionSession) Send(string, string, []ImageAttachment, []FileAttachment) error {
	return errors.New("plain Send used for plan execution")
}

func (s *planExecutionSession) SendWithCollaborationMode(prompt, messageID string, _ []ImageAttachment, _ []FileAttachment, mode string) error {
	s.sends <- planExecutionSend{prompt: prompt, mode: mode, id: messageID}
	s.events <- Event{Type: EventText, ThreadID: "thread-1", TurnID: "turn-execute", Content: "implemented"}
	s.events <- Event{Type: EventResult, ThreadID: "thread-1", TurnID: "turn-execute", SessionID: "thread-1", Content: "implemented", Done: true}
	return nil
}

func (s *planExecutionSession) RespondPermission(string, PermissionResult) error { return nil }
func (s *planExecutionSession) Events() <-chan Event                             { return s.events }
func (s *planExecutionSession) CurrentSessionID() string                         { return "thread-1" }
func (s *planExecutionSession) Alive() bool                                      { return true }
func (s *planExecutionSession) Close() error                                     { return nil }

func newMarkerQueueSession(id string) *markerQueueSession {
	return &markerQueueSession{
		controllableAgentSession: controllableAgentSession{
			sessionID: id,
			alive:     true,
			events:    make(chan Event, 16),
			closed:    make(chan struct{}),
		},
		sent: make(chan string, 4),
	}
}

func (s *markerQueueSession) Send(_ string, clientUserMessageID string, _ []ImageAttachment, _ []FileAttachment) error {
	s.sent <- clientUserMessageID
	return nil
}

func (a *mirrorSessionAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return append([]AgentSessionInfo(nil), a.listed...), nil
}

func newMirrorTestAgent(snapshot *ConversationSnapshot) *mirrorTestAgent {
	return &mirrorTestAgent{snapshot: snapshot, events: make(chan Event, 32)}
}

func (a *mirrorTestAgent) GetConversation(context.Context, string, int) (*ConversationSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	if a.readErr != nil {
		return nil, a.readErr
	}
	return a.snapshot, nil
}

func (a *mirrorTestAgent) GetConversationWindow(_ context.Context, _ string, watermark string, _ int) (*ConversationSnapshot, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	covered := watermark == ""
	if a.snapshot != nil {
		for _, turn := range a.snapshot.Turns {
			if turn.ID == watermark {
				covered = true
				break
			}
		}
	}
	return a.snapshot, covered, nil
}

func (a *mirrorTestAgent) WatchConversation(context.Context, string) (<-chan Event, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.watches++
	if a.watchErr != nil {
		return nil, a.watchErr
	}
	return a.events, nil
}

func (*mirrorTestAgent) SupportsConversationClientMarker() bool { return true }

func (a *mirrorTestAgent) InterruptConversationTurn(_ context.Context, sessionID, turnID string) error {
	a.mu.Lock()
	a.interrupts = append(a.interrupts, [2]string{sessionID, turnID})
	a.mu.Unlock()
	return nil
}

func (a *mirrorTestAgent) SteerConversationTurn(_ context.Context, sessionID, turnID, input, clientID string, _ []ImageAttachment, _ []FileAttachment) error {
	a.mu.Lock()
	a.steers = append(a.steers, [4]string{sessionID, turnID, input, clientID})
	a.mu.Unlock()
	return nil
}

func (a *mirrorTestAgent) setSnapshot(snapshot *ConversationSnapshot) {
	a.mu.Lock()
	a.snapshot = snapshot
	a.mu.Unlock()
}

func (a *mirrorTestAgent) readCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads
}

func (a *mirrorTestAgent) watchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.watches
}

type mirrorTestPlatform struct {
	stubPlatformEngine
	trackMu           sync.Mutex
	starts            []string
	startKeys         map[string]string
	updates           []string
	updateAttempts    int
	failUpdates       int
	failNotifications int
	options           []RichCardRenderOptions
	footerSends       []string
	notificationKey   map[string]struct{}
}

type mirrorQuestionResponse struct {
	requestID string
	result    PermissionResult
}

type mirrorQuestionObserver struct {
	events    chan Event
	responses chan mirrorQuestionResponse
}

type interactiveQuestionMirrorAgent struct {
	*mirrorTestAgent
	observer *mirrorQuestionObserver
	opened   chan struct{}
	once     sync.Once
}

func newInteractiveQuestionMirrorAgent(snapshot *ConversationSnapshot) *interactiveQuestionMirrorAgent {
	return &interactiveQuestionMirrorAgent{
		mirrorTestAgent: newMirrorTestAgent(snapshot),
		observer:        newMirrorQuestionObserver(),
		opened:          make(chan struct{}),
	}
}

func (a *interactiveQuestionMirrorAgent) OpenConversationObserver(context.Context, string) (ConversationObserver, error) {
	a.once.Do(func() { close(a.opened) })
	return a.observer, nil
}

func newMirrorQuestionObserver() *mirrorQuestionObserver {
	return &mirrorQuestionObserver{
		events:    make(chan Event, 8),
		responses: make(chan mirrorQuestionResponse, 8),
	}
}

func (o *mirrorQuestionObserver) Events() <-chan Event { return o.events }

func (o *mirrorQuestionObserver) RespondPermission(requestID string, result PermissionResult) error {
	o.responses <- mirrorQuestionResponse{requestID: requestID, result: result}
	return nil
}

type mirrorQuestionPlatform struct {
	*mirrorTestPlatform
	questionMu      sync.Mutex
	questionStarts  []*Card
	questionUpdates []*Card
}

func newMirrorQuestionPlatform() *mirrorQuestionPlatform {
	return &mirrorQuestionPlatform{mirrorTestPlatform: newMirrorTestPlatform()}
}

func (p *mirrorQuestionPlatform) SendTrackedCard(_ context.Context, _ any, card *Card) (any, error) {
	p.questionMu.Lock()
	defer p.questionMu.Unlock()
	handle := fmt.Sprintf("question-card-%d", len(p.questionStarts)+1)
	p.questionStarts = append(p.questionStarts, card)
	return handle, nil
}

func (p *mirrorQuestionPlatform) UpdateTrackedCard(_ context.Context, _ any, _ string, card *Card) error {
	p.questionMu.Lock()
	p.questionUpdates = append(p.questionUpdates, card)
	p.questionMu.Unlock()
	return nil
}

func (p *mirrorQuestionPlatform) questionSnapshot() ([]*Card, []*Card) {
	p.questionMu.Lock()
	defer p.questionMu.Unlock()
	starts := append([]*Card(nil), p.questionStarts...)
	updates := append([]*Card(nil), p.questionUpdates...)
	return starts, updates
}

type disabledMirrorTestPlatform struct {
	*mirrorTestPlatform
}

func (*disabledMirrorTestPlatform) SupportsConversationMirror() bool { return false }

func newMirrorTestPlatform() *mirrorTestPlatform {
	return &mirrorTestPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "mirror"},
		startKeys:          make(map[string]string),
		notificationKey:    make(map[string]struct{}),
	}
}

func (p *mirrorTestPlatform) MirrorDestinationKey(sessionKey string) (string, error) {
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid test session key")
	}
	return strings.Join(parts[:2], ":"), nil
}

func (p *mirrorTestPlatform) ReconstructReplyCtx(destination string) (any, error) {
	return destination, nil
}

func (p *mirrorTestPlatform) BuildRichCardWithOptions(options RichCardRenderOptions) string {
	p.trackMu.Lock()
	p.options = append(p.options, options)
	p.trackMu.Unlock()
	return fmt.Sprintf("%s|%s|%t|%s|%s|%v|%v|%t|%s|%s", options.Status, options.Variant, options.Streaming, options.Title, options.Markdown, options.Steps, options.ProgressItems, options.ProgressTruncated, options.Language, options.StatusFooter)
}

func (p *mirrorTestPlatform) SendPreviewStartIdempotent(_ context.Context, _ any, content, key string) (any, error) {
	p.trackMu.Lock()
	defer p.trackMu.Unlock()
	if id := p.startKeys[key]; id != "" {
		return id, nil
	}
	id := fmt.Sprintf("card-%d", len(p.startKeys)+1)
	p.startKeys[key] = id
	p.starts = append(p.starts, content)
	return id, nil
}

func (p *mirrorTestPlatform) UpdateMessage(_ context.Context, _ any, content string) error {
	p.trackMu.Lock()
	defer p.trackMu.Unlock()
	p.updateAttempts++
	if p.failUpdates > 0 {
		p.failUpdates--
		return errors.New("injected card update failure")
	}
	p.updates = append(p.updates, content)
	return nil
}

func (p *mirrorTestPlatform) EncodePreviewHandle(handle any) (string, error) {
	id, ok := handle.(string)
	if !ok || id == "" {
		return "", fmt.Errorf("invalid mirror test handle")
	}
	return id, nil
}

func (p *mirrorTestPlatform) RestorePreviewHandle(encoded string) (any, error) {
	if encoded == "" {
		return nil, fmt.Errorf("empty mirror test handle")
	}
	return encoded, nil
}

func (p *mirrorTestPlatform) PreviewMessageID(handle any) (string, error) {
	return p.EncodePreviewHandle(handle)
}

func (p *mirrorTestPlatform) SendIdempotent(ctx context.Context, replyCtx any, content, key string) error {
	p.trackMu.Lock()
	if p.failNotifications > 0 {
		p.failNotifications--
		p.trackMu.Unlock()
		return errors.New("injected notification failure")
	}
	if _, exists := p.notificationKey[key]; exists {
		p.trackMu.Unlock()
		return nil
	}
	p.notificationKey[key] = struct{}{}
	p.trackMu.Unlock()
	return p.stubPlatformEngine.Send(ctx, replyCtx, content)
}

func (p *mirrorTestPlatform) SendIdempotentWithStatusFooter(ctx context.Context, replyCtx any, content, footer, key string) error {
	p.trackMu.Lock()
	p.footerSends = append(p.footerSends, footer)
	p.trackMu.Unlock()
	return p.SendIdempotent(ctx, replyCtx, appendReplyFooter(content, footer), key)
}

func TestConversationMirror_CardFailureResultRetryPreservesFallback(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetTrackCfg(TrackCfg{Enabled: true, DefaultEnabled: true, Notify: "never", SharedWrite: "observer_only"})
	binding, err := e.bindConversationMirror(p, "mirror:chat:admin", "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	delivery, _, err := e.trackStore.claimDelivery(binding, "turn-1", "primary", "external", "")
	if err != nil {
		t.Fatalf("claimDelivery() error = %v", err)
	}
	delivery, err = e.trackStore.setDeliveryRender(delivery.Key, "hash", string(ConversationTurnCompleted), true)
	if err != nil {
		t.Fatalf("setDeliveryRender() error = %v", err)
	}
	p.failNotifications = 1
	turn := ConversationTurn{
		ID: "turn-1", Status: ConversationTurnCompleted,
		Messages: []ConversationMessage{{Role: "assistant", Content: "fallback answer", Phase: "final_answer"}},
	}
	if err := e.sendTrackTerminalResult(e.ctx, p, "mirror:chat", turn, delivery, true, false); err == nil {
		t.Fatal("first fallback result error = nil")
	}
	delivery = e.trackStore.deliveryByKey(delivery.Key)
	if !strings.HasPrefix(delivery.LastError, "terminal_card_") || delivery.NotificationState != "pending" {
		t.Fatalf("failed notification state = %#v", delivery)
	}
	if err := e.sendTrackTerminalResult(e.ctx, p, "mirror:chat", turn, delivery, strings.HasPrefix(delivery.LastError, "terminal_card_"), false); err != nil {
		t.Fatalf("fallback result retry error = %v", err)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "fallback answer") || !strings.Contains(got, "could not be updated") {
		t.Fatalf("fallback result retry = %q", got)
	}
}

func TestSendTrackTerminalResult_RepairsLegacyPlanNotificationOnce(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	binding, err := e.bindConversationMirror(p, "mirror:chat:admin", "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	delivery, _, err := e.trackStore.claimDelivery(binding, "turn-plan", "primary", "external", "")
	if err != nil {
		t.Fatalf("claimDelivery() error = %v", err)
	}
	delivery, err = e.trackStore.updateDelivery(delivery.Key, func(state *trackDeliveryState) {
		state.Terminal = true
		state.NotificationState = "sent"
		state.NotificationVersion = 0
	})
	if err != nil {
		t.Fatalf("prepare legacy delivery: %v", err)
	}
	turn := ConversationTurn{
		ID: "turn-plan", Status: ConversationTurnCompleted,
		Messages: []ConversationMessage{{Role: "assistant", Content: "# Recovered plan", Phase: "proposed_plan"}},
	}
	if err := e.sendTrackTerminalResult(e.ctx, p, "mirror:chat", turn, delivery, false, true); err != nil {
		t.Fatalf("repair legacy plan result: %v", err)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "Recovered plan") {
		t.Fatalf("repaired result = %q", got)
	}
	delivery = e.trackStore.deliveryByKey(delivery.Key)
	if delivery.NotificationVersion != trackNotificationVersion || delivery.NotificationState != "sent" {
		t.Fatalf("repaired delivery state = %#v", delivery)
	}
	before := len(p.getSent())
	if err := e.sendTrackTerminalResult(e.ctx, p, "mirror:chat", turn, delivery, false, true); err != nil {
		t.Fatalf("repeat repaired plan result: %v", err)
	}
	if after := len(p.getSent()); after != before {
		t.Fatalf("repaired notification duplicated: before=%d after=%d", before, after)
	}
}

func TestCmdTrack_RepairsPersistedTerminalPlanDelivery(t *testing.T) {
	turn := ConversationTurn{
		ID: "turn-plan", Status: ConversationTurnCompleted,
		Messages: []ConversationMessage{
			{Role: "user", Content: "make a plan"},
			{Role: "assistant", Content: "# Persisted plan\n\n- Repair it", Phase: "proposed_plan"},
		},
	}
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", turn))
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	e.SetAdminFrom("admin")
	key := "mirror:chat:admin"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("thread-1", agent.Name())
	binding, err := e.bindConversationMirror(p, key, "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	if err := e.trackStore.setInitialized(binding.Destination, turn.ID, []string{turn.ID}); err != nil {
		t.Fatalf("setInitialized() error = %v", err)
	}
	delivery, _, err := e.trackStore.claimDelivery(binding, turn.ID, "primary", "external", "")
	if err != nil {
		t.Fatalf("claimDelivery() error = %v", err)
	}
	delivery, err = e.trackStore.setDeliveryHandle(delivery.Key, "card-plan", "card-plan")
	if err != nil {
		t.Fatalf("setDeliveryHandle() error = %v", err)
	}
	_, err = e.trackStore.updateDelivery(delivery.Key, func(state *trackDeliveryState) {
		state.Terminal = true
		state.Status = string(turn.Status)
		state.RenderHash = "legacy-render"
		state.NotificationState = "sent"
		state.NotificationVersion = 0
	})
	if err != nil {
		t.Fatalf("prepare persisted delivery: %v", err)
	}

	e.ReceiveMessage(p, &Message{
		SessionKey: key, Platform: p.Name(), MessageID: "track-refresh",
		UserID: "admin", Content: "/track", ReplyCtx: "ctx",
	})
	waitMirrorTest(t, "persisted plan repair", func() bool {
		state := e.trackStore.deliveryByKey(delivery.Key)
		return state != nil && state.NotificationVersion == trackNotificationVersion
	})
	p.trackMu.Lock()
	options := append([]RichCardRenderOptions(nil), p.options...)
	updates := append([]string(nil), p.updates...)
	p.trackMu.Unlock()
	if len(updates) == 0 || len(options) == 0 || len(options[len(options)-1].Buttons) != 1 {
		t.Fatalf("persisted plan card was not refreshed with action: updates=%#v options=%#v", updates, options)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "Persisted plan") || !strings.Contains(got, "refreshed") {
		t.Fatalf("persisted plan repair output = %q", got)
	}
}

func waitMirrorTest(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func mirrorTestSnapshot(threadID string, turn ConversationTurn) *ConversationSnapshot {
	turns := []ConversationTurn(nil)
	if turn.ID != "" {
		turns = []ConversationTurn{turn}
	}
	return &ConversationSnapshot{SessionID: threadID, ThreadState: "active", Turns: turns, RetrievedAt: time.Now()}
}

func startMirrorTest(t *testing.T, agent *mirrorTestAgent, p *mirrorTestPlatform) (*Engine, *trackBindingState, string) {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", agent, []Platform{p}, storePath, LangEnglish)
	e.SetAdminFrom("admin")
	t.Cleanup(func() {
		e.cancel()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			e.trackMu.Lock()
			running := len(e.conversationMirrors)
			e.trackMu.Unlock()
			if running == 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	e.sessions.Save()
	binding, err := e.bindConversationMirror(p, key, "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	e.startConversationMirror(agent, e.sessions, p, binding)
	waitMirrorTest(t, "initial mirror reconciliation", func() bool { return agent.readCount() > 0 })
	return e, binding, key
}

func firstCardActionWithPrefix(card *Card, prefix string) string {
	if card == nil {
		return ""
	}
	for _, element := range card.Elements {
		switch item := element.(type) {
		case CardListItem:
			if strings.HasPrefix(item.BtnValue, prefix) {
				return item.BtnValue
			}
		case CardActions:
			for _, button := range item.Buttons {
				if strings.HasPrefix(button.Value, prefix) {
					return button.Value
				}
			}
		}
	}
	return ""
}

func TestConversationMirror_ForegroundDeliveryWithoutBusySessionShowsRecoveredQuestion(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorQuestionPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	defer e.cancel()
	e.SetTrackCfg(TrackCfg{Enabled: true, DefaultEnabled: true, Notify: "never", SharedWrite: "observer_only"})
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	binding, err := e.bindConversationMirror(p, key, "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	if _, _, err := e.trackStore.claimDelivery(binding, "turn-lifecycle", "primary", "foreground", "task-message"); err != nil {
		t.Fatalf("claim foreground delivery: %v", err)
	}

	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	mirror := &conversationMirror{
		cancel: cancel, destination: binding.Destination, sessionKey: binding.SessionKey,
		threadID: binding.ThreadID, generation: binding.Generation, wake: make(chan struct{}, 1),
		platform: p, handles: make(map[string]any),
	}
	question := Event{
		Type: EventPermissionRequest, ThreadID: "thread-1", TurnID: "turn-lifecycle", ItemID: "question-1",
		RequestID: "request-1", ToolName: "AskUserQuestion",
		Questions: []UserQuestion{{Question: "Which database?", Options: []UserQuestionOption{{Label: "Postgres"}}}},
	}
	e.handleConversationElicitation(ctx, mirror, e.sessions, p, newMirrorQuestionObserver(), question)
	e.handleConversationElicitation(ctx, mirror, e.sessions, p, newMirrorQuestionObserver(), question)

	starts, _ := p.questionSnapshot()
	if len(starts) != 1 || !strings.Contains(starts[0].RenderText(), "Which database?") {
		t.Fatalf("recovered lifecycle question cards = %#v, want one actionable question", starts)
	}
}

func TestConversationMirror_BlockingQuestionIsVisibleAnswerableAndInvalidated(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorQuestionPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	defer e.cancel()
	e.SetAdminFrom("admin")
	e.SetTrackCfg(TrackCfg{Enabled: true, DefaultEnabled: true, Notify: "never", SharedWrite: "observer_only"})
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	binding, err := e.bindConversationMirror(p, key, "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	mirror := &conversationMirror{
		cancel: cancel, destination: binding.Destination, sessionKey: binding.SessionKey,
		threadID: binding.ThreadID, generation: binding.Generation, wake: make(chan struct{}, 1),
		platform: p, handles: make(map[string]any),
	}
	e.trackMu.Lock()
	e.conversationMirrors[binding.Destination] = mirror
	e.trackMu.Unlock()
	observer := newMirrorQuestionObserver()
	questions := []UserQuestion{{
		Question: "Which database?", Header: "Database",
		Options: []UserQuestionOption{{Label: "Postgres", Description: "Production"}, {Label: "SQLite", Description: "Embedded"}},
	}}

	// Action 1: the shared external turn blocks and Feishu receives the same
	// native AskUserQuestion card shape, with exact mirror action tokens.
	e.handleConversationElicitation(ctx, mirror, e.sessions, p, observer, Event{
		Type: EventPermissionRequest, ThreadID: "thread-1", TurnID: "turn-external", ItemID: "question-1",
		RequestID: "request-1", ToolName: "AskUserQuestion", Questions: questions,
	})
	starts, _ := p.questionSnapshot()
	if len(starts) != 1 {
		t.Fatalf("tracked question cards = %d, want 1", len(starts))
	}
	card := starts[0]
	if card.Header == nil || card.Header.Color != "blue" || card.Header.Title != e.i18n.T(MsgAskQuestionTitle) {
		t.Fatalf("question card header = %#v", card.Header)
	}
	if got := card.RenderText(); !strings.Contains(got, "Which database?") || !strings.Contains(got, "Postgres") || !strings.Contains(got, "SQLite") {
		t.Fatalf("question card text = %q", got)
	}
	if got := countCardActionValues(card, "trackq:"); got != 2 {
		t.Fatalf("trackq controls = %d, want 2", got)
	}
	action := firstCardActionWithPrefix(card, "trackq:")
	if action == "" {
		t.Fatal("question card has no trackq action")
	}

	// Action 2: a typed look-alike and a copied-card message ID both fail
	// closed. Only a real callback from the exact source card reaches Codex.
	if e.handleTrackedConversationElicitationInput(p, &Message{
		SessionKey: key, UserID: "admin", Content: action, ReplyCtx: "ctx",
	}, action, e.sessions) {
		t.Fatal("typed trackq look-alike was consumed as a card action")
	}
	if !e.handleTrackedConversationElicitationInput(p, &Message{
		SessionKey: key, UserID: "admin", Content: action, ReplyCtx: "ctx",
		ReferencedMessageID: "copied-card", IsCardAction: true,
	}, action, e.sessions) {
		t.Fatal("copied-card action should be consumed as stale")
	}
	select {
	case response := <-observer.responses:
		t.Fatalf("invalid card action reached observer: %#v", response)
	default:
	}
	e.ReceiveMessage(p, &Message{
		SessionKey: key, Platform: p.Name(), MessageID: "answer-1", UserID: "admin",
		Content: action, ReplyCtx: "ctx", ReferencedMessageID: "question-card-1", IsCardAction: true,
	})
	select {
	case response := <-observer.responses:
		if response.requestID != "request-1" || response.result.Behavior != "allow" {
			t.Fatalf("observer response = %#v", response)
		}
		answers, ok := response.result.UpdatedInput["answers"].(map[string]any)
		if !ok || answers["Which database?"] != "Postgres" {
			t.Fatalf("observer answers = %#v", response.result.UpdatedInput)
		}
	case <-time.After(time.Second):
		t.Fatal("exact question action did not answer observer")
	}
	_, updates := p.questionSnapshot()
	if len(updates) == 0 || updates[len(updates)-1].Header == nil || updates[len(updates)-1].Header.Color != "green" || updates[len(updates)-1].HasButtons() {
		t.Fatalf("answered question card = %#v", updates)
	}
	localUpdateCount := len(updates)
	e.resolveConversationElicitation(mirror, p, "request-1", "")
	_, updates = p.questionSnapshot()
	if len(updates) != localUpdateCount || updates[len(updates)-1].Header.Color != "green" {
		t.Fatalf("daemon resolution overwrote the locally answered card: %#v", updates)
	}

	// Action 3: if a TUI answers a later question first, the Feishu controls
	// disappear and explain the cross-client resolution instead of hanging.
	e.handleConversationElicitation(ctx, mirror, e.sessions, p, observer, Event{
		Type: EventPermissionRequest, ThreadID: "thread-1", TurnID: "turn-external-2", ItemID: "question-2",
		RequestID: "request-2", ToolName: "AskUserQuestion", Questions: questions,
	})
	e.resolveConversationElicitation(mirror, p, "request-2", "")
	starts, updates = p.questionSnapshot()
	if len(starts) != 2 || len(updates) < 2 {
		t.Fatalf("question lifecycle starts=%d updates=%d", len(starts), len(updates))
	}
	resolved := updates[len(updates)-1]
	if resolved.Header == nil || resolved.Header.Color != "grey" || resolved.HasButtons() ||
		!strings.Contains(resolved.RenderText(), "another client") {
		t.Fatalf("externally resolved question card = %#v text=%q", resolved, resolved.RenderText())
	}

	// A foreground cc-connect turn renders the native prompt itself; the
	// observer copy must not create a duplicate mirror question card.
	if !session.TryLock() {
		t.Fatal("failed to mark foreground session busy")
	}
	e.handleConversationElicitation(ctx, mirror, e.sessions, p, observer, Event{
		Type: EventPermissionRequest, ThreadID: "thread-1", TurnID: "turn-foreground", ItemID: "question-3",
		RequestID: "request-3", ToolName: "AskUserQuestion", Questions: questions,
	})
	session.UnlockWithoutUpdate()
	starts, _ = p.questionSnapshot()
	if len(starts) != 2 {
		t.Fatalf("foreground request created duplicate mirror card: %d", len(starts))
	}

	// Sensitive answers are never collected or echoed through a shared chat.
	secretQuestions := []UserQuestion{{Question: "Enter the deployment token", Secret: true}}
	e.handleConversationElicitation(ctx, mirror, e.sessions, p, observer, Event{
		Type: EventPermissionRequest, ThreadID: "thread-1", TurnID: "turn-external-4", ItemID: "question-4",
		RequestID: "request-4", ToolName: "AskUserQuestion", Questions: secretQuestions,
	})
	starts, _ = p.questionSnapshot()
	if len(starts) != 3 || starts[len(starts)-1].HasButtons() || !strings.Contains(starts[len(starts)-1].RenderText(), "sensitive answer") {
		t.Fatalf("sensitive external question card = %#v", starts)
	}
	e.ReceiveMessage(p, &Message{
		SessionKey: key, Platform: p.Name(), MessageID: "secret-answer", UserID: "admin",
		Content: "top-secret", ReplyCtx: "ctx", ReferencedMessageID: "question-card-3",
	})
	select {
	case response := <-observer.responses:
		t.Fatalf("sensitive answer reached shared observer: %#v", response)
	default:
	}
	if got := strings.Join(p.getSent(), "\n"); strings.Contains(got, "top-secret") || !strings.Contains(got, "another client") {
		t.Fatalf("sensitive answer handling leaked or omitted warning: %q", got)
	}
	e.resolveConversationElicitation(mirror, p, "request-4", "")

	// A terminal authoritative snapshot also retires the prompt, even when a
	// resumed observer did not receive the original turn/started event.
	e.handleConversationElicitation(ctx, mirror, e.sessions, p, observer, Event{
		Type: EventPermissionRequest, ThreadID: "thread-1", TurnID: "turn-external-5", ItemID: "question-5",
		RequestID: "request-5", ToolName: "AskUserQuestion", Questions: questions,
	})
	agent.setSnapshot(mirrorTestSnapshot("thread-1", ConversationTurn{ID: "turn-external-5", Status: ConversationTurnCompleted}))
	if err := e.reconcileConversationMirror(ctx, mirror, agent, e.sessions, p); err != nil {
		t.Fatalf("terminal snapshot reconciliation error = %v", err)
	}
	starts, updates = p.questionSnapshot()
	if len(starts) != 4 || len(updates) == 0 || updates[len(updates)-1].Header == nil || updates[len(updates)-1].Header.Color != "grey" {
		t.Fatalf("terminal snapshot left active question controls: starts=%d updates=%#v", len(starts), updates)
	}

	// Disabling or rebinding tracking also retires any outstanding controls.
	e.handleConversationElicitation(ctx, mirror, e.sessions, p, observer, Event{
		Type: EventPermissionRequest, ThreadID: "thread-1", TurnID: "turn-external-6", ItemID: "question-6",
		RequestID: "request-6", ToolName: "AskUserQuestion", Questions: questions,
	})
	e.stopConversationMirror(binding.Destination)
	starts, updates = p.questionSnapshot()
	if len(starts) != 5 || len(updates) == 0 || updates[len(updates)-1].Header == nil || updates[len(updates)-1].Header.Color != "grey" {
		t.Fatalf("retired mirror left active question controls: starts=%d updates=%#v", len(starts), updates)
	}
}

func (*trackChatAgent) Name() string { return "codex-test" }

func (a *trackChatAgent) StartSession(_ context.Context, sessionID string) (AgentSession, error) {
	a.mu.Lock()
	a.startSessionID = sessionID
	a.mu.Unlock()
	return a.session, nil
}

func (*trackChatAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) { return nil, nil }
func (*trackChatAgent) Stop() error                                              { return nil }

func (a *trackChatAgent) GetConversation(context.Context, string, int) (*ConversationSnapshot, error) {
	return a.snapshot, nil
}

func (a *trackChatAgent) startedWith() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startSessionID
}

func newTrackPreviewPlatform() *trackPreviewPlatform {
	return &trackPreviewPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "feishu"},
		starts:             make(chan string, 4),
		updates:            make(chan string, 4),
	}
}

func (p *trackPreviewPlatform) SendPreviewStart(_ context.Context, _ any, content string) (any, error) {
	p.starts <- content
	return "track-card", nil
}

func (p *trackPreviewPlatform) UpdateMessage(_ context.Context, _ any, content string) error {
	p.updates <- content
	return nil
}

func TestCmdHistory_ConversationProviderIsSoleTruth(t *testing.T) {
	started := time.Unix(100, 0)
	completed := time.Unix(105, 0)
	agent := &authoritativeConversationAgent{snapshots: []*ConversationSnapshot{{
		SessionID: "thread-1",
		Turns: []ConversationTurn{{
			ID: "turn-1", Status: ConversationTurnCompleted, StartedAt: started, CompletedAt: completed,
			Messages: []ConversationMessage{
				{Role: "user", Content: "authoritative prompt"},
				{Role: "assistant", Content: "interim detail", Phase: "commentary"},
				{Role: "assistant", Content: "authoritative answer", Phase: "final_answer"},
			},
		}},
	}}}
	p := &stubPlatformEngine{n: "feishu"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetAdminFrom("admin")
	session := e.sessions.GetOrCreateActive("feishu:chat:admin")
	session.SetAgentSessionID("thread-1", "codex")
	session.AddHistory("user", "local-only secret")
	session.AddHistory("assistant", "stale local answer")

	e.handleCommand(p, &Message{SessionKey: "feishu:chat:admin", Platform: "feishu", UserID: "admin", ReplyCtx: "ctx"}, "/history")

	got := strings.Join(p.getSent(), "\n")
	for _, want := range []string{"authoritative prompt", "authoritative answer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("history output %q does not contain %q", got, want)
		}
	}
	for _, forbidden := range []string{"local-only secret", "stale local answer", "interim detail"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("history output %q unexpectedly contains %q", got, forbidden)
		}
	}
}

func TestCmdHistory_ConversationProviderFailsClosed(t *testing.T) {
	agent := &authoritativeConversationAgent{err: errors.New("daemon unavailable")}
	p := &stubPlatformEngine{n: "feishu"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetAdminFrom("admin")
	session := e.sessions.GetOrCreateActive("feishu:chat:admin")
	session.SetAgentSessionID("thread-1", "codex")
	session.AddHistory("assistant", "must not leak from local storage")

	e.handleCommand(p, &Message{SessionKey: "feishu:chat:admin", Platform: "feishu", UserID: "admin", ReplyCtx: "ctx"}, "/history")

	got := strings.Join(p.getSent(), "\n")
	if !strings.Contains(got, "daemon unavailable") {
		t.Fatalf("history output = %q, want backend error", got)
	}
	if strings.Contains(got, "must not leak") {
		t.Fatalf("history fell back to local storage: %q", got)
	}
}

func TestCmdHistory_ConversationProviderRequiresAdmin(t *testing.T) {
	agent := &authoritativeConversationAgent{snapshots: []*ConversationSnapshot{{SessionID: "thread-1"}}}
	p := &stubPlatformEngine{n: "feishu"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetAdminFrom("admin")
	e.sessions.GetOrCreateActive("feishu:group:user").SetAgentSessionID("thread-1", "codex")

	e.handleCommand(p, &Message{SessionKey: "feishu:group:user", Platform: "feishu", UserID: "user", ReplyCtx: "ctx"}, "/history")

	got := strings.Join(p.getSent(), "\n")
	if !strings.Contains(got, "requires admin") {
		t.Fatalf("history output = %q, want admin rejection", got)
	}
	if agent.callCount() != 0 {
		t.Fatalf("backend calls = %d, want 0 before authorization", agent.callCount())
	}
}

func TestCmdTrack_UpdatesPinnedLatestTurnUntilCompletion(t *testing.T) {
	started := time.Now().Add(-time.Second)
	running := &ConversationSnapshot{
		SessionID: "thread-1", ThreadState: "active", ActiveFlags: []string{"waitingOnUserInput"},
		Turns: []ConversationTurn{{
			ID: "turn-1", Status: ConversationTurnInProgress, StartedAt: started,
			Messages: []ConversationMessage{
				{Role: "user", Content: "build the feature"},
				{Role: "assistant", Content: "Inspecting the code.", Phase: "commentary"},
			},
			Activities: []ConversationActivity{{Kind: "shell", Status: "inProgress"}},
		}},
	}
	completed := &ConversationSnapshot{
		SessionID: "thread-1", ThreadState: "idle",
		Turns: []ConversationTurn{{
			ID: "turn-1", Status: ConversationTurnCompleted, StartedAt: started, CompletedAt: time.Now(),
			Messages: []ConversationMessage{
				{Role: "user", Content: "build the feature"},
				{Role: "assistant", Content: "Inspecting the code.", Phase: "commentary"},
				{Role: "assistant", Content: "Feature built.", Phase: "final_answer"},
			},
			Activities: []ConversationActivity{{Kind: "shell", Status: "completed"}},
		}},
	}
	agent := &authoritativeConversationAgent{snapshots: []*ConversationSnapshot{running, completed}}
	p := newTrackPreviewPlatform()
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	defer e.cancel()
	e.SetAdminFrom("admin")
	e.sessions.GetOrCreateActive("feishu:chat:admin").SetAgentSessionID("thread-1", "codex")

	e.handleCommand(p, &Message{SessionKey: "feishu:chat:admin", Platform: "feishu", UserID: "admin", ReplyCtx: "ctx"}, "/track")

	select {
	case initial := <-p.starts:
		for _, want := range []string{"build the feature", "Inspecting the code.", "Running", "user input"} {
			if !strings.Contains(initial, want) {
				t.Fatalf("initial track card %q does not contain %q", initial, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial track card")
	}

	select {
	case update := <-p.updates:
		if !strings.Contains(update, "Feature built.") || !strings.Contains(update, "Completed") {
			t.Fatalf("completed track card = %q", update)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for completed track update")
	}
}

func TestCmdTrack_RunningCardShowsAndRefreshesStatusConfirmation(t *testing.T) {
	confirmed1 := time.Date(2026, 8, 30, 12, 0, 1, 0, time.Local)
	confirmed2 := confirmed1.Add(time.Minute)
	visibleConfirmed1 := confirmed1.Truncate(trackHealthDisplayInterval)
	visibleConfirmed2 := confirmed2.Truncate(trackHealthDisplayInterval)
	running1 := &ConversationSnapshot{
		SessionID: "thread-1", RetrievedAt: confirmed1,
		Turns: []ConversationTurn{{
			ID: "turn-1", Status: ConversationTurnInProgress,
			Messages: []ConversationMessage{{Role: "user", Content: "long task"}},
		}},
	}
	running2 := &ConversationSnapshot{
		SessionID: "thread-1", RetrievedAt: confirmed2,
		Turns: []ConversationTurn{{
			ID: "turn-1", Status: ConversationTurnInProgress,
			Messages: []ConversationMessage{
				{Role: "user", Content: "long task"},
				{Role: "assistant", Content: "still working", Phase: "commentary"},
			},
		}},
	}
	agent := &authoritativeConversationAgent{snapshots: []*ConversationSnapshot{running1, running2}}
	p := newTrackHealthPreviewPlatform()
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	defer e.cancel()
	e.SetAdminFrom("admin")
	key := "feishu:chat:admin"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("thread-1", "codex")

	e.handleCommand(p, &Message{SessionKey: key, Platform: "feishu", UserID: "admin", ReplyCtx: "ctx"}, "/track")
	options := p.getOptions()
	if len(options) == 0 || !strings.Contains(options[0].StatusFooter, "Task status confirmed at "+visibleConfirmed1.Format("15:04:05")) {
		t.Fatalf("initial track health options = %#v", options)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		options = p.getOptions()
		if len(options) >= 2 && strings.Contains(options[len(options)-1].StatusFooter, "Task status confirmed at "+visibleConfirmed2.Format("15:04:05")) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("track confirmation was not refreshed: options=%#v calls=%d", options, agent.callCount())
}

func TestConversationTracker_ReadFailureIsVisibleOnReplacementCard(t *testing.T) {
	p := newTrackHealthPreviewPlatform()
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	turn := ConversationTurn{
		ID: "turn-1", Status: ConversationTurnInProgress,
		Messages: []ConversationMessage{{Role: "user", Content: "long task"}},
	}
	confirmedAt := time.Date(2026, 8, 30, 12, 0, 1, 0, time.Local)
	snapshot := &ConversationSnapshot{SessionID: "thread-1", RetrievedAt: confirmedAt, Turns: []ConversationTurn{turn}}
	tracker := &conversationTracker{
		sessionID: "thread-1", turnID: "turn-1", health: ProgressCardHealthVerified,
		lastVerifiedAt: confirmedAt, lastSnapshot: snapshot, lastTurn: turn,
	}
	markdown := e.renderTrackMarkdown(snapshot, turn)
	lastPayload := e.renderTrackPayloadWithHealth(p, snapshot, turn, markdown, "feishu:chat:admin", true, tracker.health, confirmedAt)

	now := confirmedAt.Add(time.Second)
	e.updateConversationTrackerHealth(e.ctx, tracker, "feishu:chat:admin", p, p, "card", &lastPayload, now)
	options := p.getOptions()
	last := options[len(options)-1]
	if tracker.health != ProgressCardHealthReconnecting || !strings.Contains(last.StatusFooter, "Reconnecting to task status") {
		t.Fatalf("reconnecting replacement card = tracker %#v options %#v", tracker, options)
	}

	tracker.firstFailureAt = now.Add(-cardHealthUnknownAfter)
	e.updateConversationTrackerHealth(e.ctx, tracker, "feishu:chat:admin", p, p, "card", &lastPayload, now)
	options = p.getOptions()
	last = options[len(options)-1]
	if tracker.health != ProgressCardHealthUnknown || !strings.Contains(last.StatusFooter, "Task status unknown") {
		t.Fatalf("unknown replacement card = tracker %#v options %#v", tracker, options)
	}
}

func TestConversationTracker_RecoversVisibleConfirmationAfterReadFailure(t *testing.T) {
	confirmed1 := time.Date(2026, 8, 30, 12, 0, 1, 0, time.Local)
	confirmed2 := confirmed1.Add(time.Minute)
	turn := ConversationTurn{
		ID: "turn-1", Status: ConversationTurnInProgress,
		Messages: []ConversationMessage{{Role: "user", Content: "long task"}},
	}
	agent := newMirrorTestAgent(&ConversationSnapshot{SessionID: "thread-1", RetrievedAt: confirmed1, Turns: []ConversationTurn{turn}})
	p := newTrackHealthPreviewPlatform()
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	defer e.cancel()
	e.SetAdminFrom("admin")
	key := "feishu:chat:admin"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("thread-1", agent.Name())

	e.handleCommand(p, &Message{SessionKey: key, Platform: "feishu", UserID: "admin", ReplyCtx: "ctx"}, "/track")
	agent.mu.Lock()
	agent.readErr = errors.New("temporary backend failure")
	agent.mu.Unlock()
	waitMirrorTest(t, "replacement card reconnecting state", func() bool {
		options := p.getOptions()
		return len(options) > 0 && strings.Contains(options[len(options)-1].StatusFooter, "Reconnecting to task status")
	})

	agent.setSnapshot(&ConversationSnapshot{SessionID: "thread-1", RetrievedAt: confirmed2, Turns: []ConversationTurn{turn}})
	agent.mu.Lock()
	agent.readErr = nil
	agent.mu.Unlock()
	want := "Task status confirmed at " + confirmed2.Truncate(trackHealthDisplayInterval).Format("15:04:05")
	waitMirrorTest(t, "replacement card confirmed after recovery", func() bool {
		options := p.getOptions()
		return len(options) > 0 && strings.Contains(options[len(options)-1].StatusFooter, want)
	})
}

func TestCmdTrack_DoesNotBlockFollowingFeishuConversation(t *testing.T) {
	snapshot := &ConversationSnapshot{
		SessionID: "thread-1",
		Turns: []ConversationTurn{{
			ID: "turn-1", Status: ConversationTurnCompleted,
			Messages: []ConversationMessage{
				{Role: "user", Content: "previous prompt"},
				{Role: "assistant", Content: "previous answer", Phase: "final_answer"},
			},
		}},
	}
	agent := &trackChatAgent{snapshot: snapshot, session: newResultAgentSession("new Feishu answer")}
	p := newTrackPreviewPlatform()
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	e.SetAdminFrom("admin")
	key := "feishu:chat:admin"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("thread-1", "codex")

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "feishu", MessageID: "track", UserID: "admin", Content: "/track", ReplyCtx: "ctx"})
	select {
	case <-p.starts:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for track card")
	}

	e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "feishu", MessageID: "chat", UserID: "admin", Content: "continue in Feishu", ReplyCtx: "ctx"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(strings.Join(p.getSent(), "\n"), "new Feishu answer") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "new Feishu answer") {
		t.Fatalf("normal Feishu reply missing after /track: %q", got)
	}
	if got := agent.startedWith(); got != "thread-1" {
		t.Fatalf("normal message resumed session %q, want thread-1", got)
	}
}

func TestCmdTrack_RunningCardCarriesExactInterruptAction(t *testing.T) {
	agent := &authoritativeConversationAgent{snapshots: []*ConversationSnapshot{{
		SessionID: "thread-1",
		Turns: []ConversationTurn{{
			ID: "turn-1", Status: ConversationTurnInProgress,
			Messages: []ConversationMessage{{Role: "user", Content: "prompt"}},
		}},
	}}}
	p := &trackActionPlatform{trackPreviewPlatform: newTrackPreviewPlatform()}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	defer e.cancel()
	e.SetAdminFrom("admin")
	key := "feishu:chat:admin"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("thread-1", "codex")

	e.handleCommand(p, &Message{SessionKey: key, Platform: "feishu", UserID: "admin", ReplyCtx: "ctx"}, "/track")

	p.mu.Lock()
	buttons := append([]CardButton(nil), p.buttons...)
	p.mu.Unlock()
	if len(buttons) != 1 {
		t.Fatalf("track buttons = %#v, want one interrupt button", buttons)
	}
	if got, want := buttons[0].Value, "cmd:/track stop thread-1 turn-1"; got != want {
		t.Fatalf("interrupt action = %q, want %q", got, want)
	}
	if got := buttons[0].Extra["session_key"]; got != key {
		t.Fatalf("interrupt session key = %q, want %q", got, key)
	}
}

func TestRenderTrackPayload_CompletedPlanCarriesExactExecuteAction(t *testing.T) {
	p := newMirrorTestPlatform()
	e := NewEngine("test", &stubAgent{}, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	key := "mirror:chat:admin"
	binding, err := e.bindConversationMirror(p, key, "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	delivery, _, err := e.trackStore.claimDelivery(binding, "turn-plan", "primary", "external", "")
	if err != nil {
		t.Fatalf("claimDelivery() error = %v", err)
	}
	delivery, err = e.trackStore.setDeliveryRender(delivery.Key, "", string(ConversationTurnCompleted), true)
	if err != nil {
		t.Fatalf("setDeliveryRender() error = %v", err)
	}
	turn := ConversationTurn{
		ID: "turn-plan", Status: ConversationTurnCompleted,
		Messages: []ConversationMessage{
			{Role: "user", Content: "design it"},
			{Role: "assistant", Content: "# Plan\n\n- Build it", Phase: "proposed_plan"},
		},
	}
	snapshot := mirrorTestSnapshot("thread-1", turn)
	e.renderTrackPayloadWithResponse(p, snapshot, turn, "fallback", key, false)

	p.trackMu.Lock()
	options := append([]RichCardRenderOptions(nil), p.options...)
	p.trackMu.Unlock()
	if len(options) != 1 || len(options[0].Buttons) != 1 {
		t.Fatalf("plan card options = %#v", options)
	}
	button := options[0].Buttons[0]
	if button.Value != trackPlanExecutePrefix+delivery.Key || button.Extra["session_key"] != key || button.Type != "primary" {
		t.Fatalf("plan execute button = %#v", button)
	}
	if !strings.Contains(options[0].StatusFooter, "start its implementation") {
		t.Fatalf("plan execute footer = %q", options[0].StatusFooter)
	}
}

func TestTrackedPlanAction_StartsOneExactDefaultModeTurn(t *testing.T) {
	turn := ConversationTurn{
		ID: "turn-plan", Status: ConversationTurnCompleted,
		Messages: []ConversationMessage{
			{Role: "user", Content: "make a plan"},
			{Role: "assistant", Content: "# Plan\n\n- Build it", Phase: "proposed_plan"},
		},
	}
	base := newMirrorTestAgent(mirrorTestSnapshot("thread-1", turn))
	execution := newPlanExecutionSession()
	agent := &interactiveMirrorAgent{mirrorTestAgent: base, session: execution}
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	e.SetAdminFrom("admin")
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	binding, err := e.bindConversationMirror(p, key, "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	delivery, _, err := e.trackStore.claimDelivery(binding, turn.ID, "primary", "external", "")
	if err != nil {
		t.Fatalf("claimDelivery() error = %v", err)
	}
	delivery, err = e.trackStore.setDeliveryHandle(delivery.Key, "card-plan", "card-plan")
	if err != nil {
		t.Fatalf("setDeliveryHandle() error = %v", err)
	}
	delivery, err = e.trackStore.setDeliveryRender(delivery.Key, "hash", string(turn.Status), true)
	if err != nil {
		t.Fatalf("setDeliveryRender() error = %v", err)
	}
	action := trackPlanExecutePrefix + delivery.Key

	// A copied or otherwise mismatched source card fails before the one-shot
	// action is claimed.
	e.ReceiveMessage(p, &Message{
		SessionKey: key, Platform: p.Name(), ReferencedMessageID: "another-card",
		UserID: "admin", Content: action, ReplyCtx: "ctx", IsCardAction: true,
	})
	if got := e.trackStore.deliveryByKey(delivery.Key).PlanActionState; got != "" {
		t.Fatalf("mismatched card claimed plan action: %q", got)
	}

	e.ReceiveMessage(p, &Message{
		SessionKey: key, Platform: p.Name(), ReferencedMessageID: "card-plan",
		UserID: "admin", UserName: "Admin", Content: action, ReplyCtx: "ctx", IsCardAction: true,
	})
	select {
	case sent := <-execution.sends:
		if sent.mode != "default" || sent.id == "" || !strings.Contains(sent.prompt, "immediately preceding turn") {
			t.Fatalf("plan execution send = %#v", sent)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for plan execution turn")
	}
	waitMirrorTest(t, "accepted plan action", func() bool {
		return e.trackStore.deliveryByKey(delivery.Key).PlanActionState == "accepted"
	})

	// Repeated delivery of the same callback must not start a second turn.
	e.ReceiveMessage(p, &Message{
		SessionKey: key, Platform: p.Name(), ReferencedMessageID: "card-plan",
		UserID: "admin", Content: action, ReplyCtx: "ctx", IsCardAction: true,
	})
	select {
	case sent := <-execution.sends:
		t.Fatalf("duplicate plan execution turn = %#v", sent)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestConversationFinalAnswer_UsesProposedPlan(t *testing.T) {
	turn := ConversationTurn{Messages: []ConversationMessage{{
		Role: "assistant", Phase: "proposed_plan", Content: "# Plan\n\n- Visible",
	}}}
	if got := conversationFinalAnswer(turn); got != "# Plan\n\n- Visible" {
		t.Fatalf("conversationFinalAnswer() = %q", got)
	}
}

func TestRenderTrackMarkdown_TruncatesLongUTF8Sections(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	longPrompt := strings.Repeat("提示", trackSectionMaxBytes)
	longResponse := strings.Repeat("回答", trackSectionMaxBytes)
	markdown := e.renderTrackMarkdown(&ConversationSnapshot{}, ConversationTurn{
		Status: ConversationTurnInProgress,
		Messages: []ConversationMessage{
			{Role: "user", Content: longPrompt},
			{Role: "assistant", Content: longResponse, Phase: "commentary"},
		},
	})

	if !utf8.ValidString(markdown) {
		t.Fatal("track markdown contains invalid UTF-8 after truncation")
	}
	if got := strings.Count(markdown, "Content truncated to fit the card."); got != 2 {
		t.Fatalf("truncation markers = %d, want 2", got)
	}
	if len(markdown) > 2*trackSectionMaxBytes+1_000 {
		t.Fatalf("track markdown size = %d, expected bounded output", len(markdown))
	}
}

func TestCmdTrackStop_InterruptsOnlyMatchingLiveTracker(t *testing.T) {
	agent := &authoritativeConversationAgent{snapshots: []*ConversationSnapshot{{
		SessionID: "thread-1",
		Turns:     []ConversationTurn{{ID: "turn-1", Status: ConversationTurnInProgress}},
	}}}
	p := &stubPlatformEngine{n: "feishu"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.SetAdminFrom("admin")
	key := "feishu:chat:admin"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("thread-1", "codex")
	e.trackers[key] = &conversationTracker{cancel: func() {}, sessionID: "thread-1", turnID: "turn-1"}

	e.handleCommand(p, &Message{SessionKey: key, Platform: "feishu", UserID: "admin", ReplyCtx: "ctx"}, "/track stop thread-1 turn-1")

	agent.mu.Lock()
	interrupts := append([][2]string(nil), agent.interrupts...)
	agent.mu.Unlock()
	if len(interrupts) != 1 || interrupts[0] != [2]string{"thread-1", "turn-1"} {
		t.Fatalf("interrupts = %#v", interrupts)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "Interrupt requested") {
		t.Fatalf("interrupt reply = %q", got)
	}

	p.clearSent()
	e.handleCommand(p, &Message{SessionKey: key, Platform: "feishu", UserID: "admin", ReplyCtx: "ctx"}, "/track stop thread-1 turn-old")
	agent.mu.Lock()
	interruptCount := len(agent.interrupts)
	agent.mu.Unlock()
	if interruptCount != 1 {
		t.Fatalf("stale card caused another interrupt: %#v", agent.interrupts)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "stale") {
		t.Fatalf("stale interrupt reply = %q", got)
	}
}

func TestConversationMirror_DefaultOnCreatesProgressCardAndFinalResult(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e, binding, _ := startMirrorTest(t, agent, p)
	started := time.Now().Add(-time.Second)
	running := ConversationTurn{
		ID: "turn-external", Status: ConversationTurnInProgress, StartedAt: started,
		Messages:   []ConversationMessage{{Role: "user", Content: "external prompt"}, {Role: "assistant", Content: "working", Phase: "commentary"}},
		Activities: []ConversationActivity{{ID: "tool-1", Kind: "shell", Name: "Bash", Summary: "ls", Status: "in_progress"}},
	}
	agent.setSnapshot(mirrorTestSnapshot("thread-1", running))
	agent.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: running.ID}
	waitMirrorTest(t, "external mirror card", func() bool {
		p.trackMu.Lock()
		defer p.trackMu.Unlock()
		return len(p.starts) == 1
	})

	p.trackMu.Lock()
	firstOptions := p.options[0]
	p.trackMu.Unlock()
	if firstOptions.Variant != CardVariantMirror || firstOptions.Status != CardStatusWorking || !firstOptions.Streaming {
		t.Fatalf("running mirror options = %#v", firstOptions)
	}
	if !strings.Contains(firstOptions.Markdown, "external prompt") || strings.Contains(firstOptions.Markdown, "working") || !strings.Contains(firstOptions.Title, "Shared external") {
		t.Fatalf("running mirror presentation = %#v", firstOptions)
	}
	if len(firstOptions.ProgressItems) != 2 || firstOptions.ProgressItems[0].Kind != ProgressEntryThinking || firstOptions.ProgressItems[1].Kind != ProgressEntryToolUse {
		t.Fatalf("running mirror progress = %#v", firstOptions.ProgressItems)
	}
	if firstOptions.ProgressCounts.Reasoning != 1 || firstOptions.ProgressCounts.Tools != 1 {
		t.Fatalf("running mirror cumulative counts = %#v", firstOptions.ProgressCounts)
	}

	completed := running
	completed.Status = ConversationTurnCompleted
	completed.CompletedAt = time.Now()
	completed.Messages = append(completed.Messages, ConversationMessage{Role: "assistant", Content: "external answer", Phase: "final_answer"})
	completed.Activities[0].Status = "completed"
	agent.setSnapshot(&ConversationSnapshot{SessionID: "thread-1", ThreadState: "idle", Turns: []ConversationTurn{completed}})
	agent.events <- Event{Type: EventResult, ThreadID: "thread-1", TurnID: completed.ID, Done: true}
	waitMirrorTest(t, "terminal card and final result", func() bool {
		p.trackMu.Lock()
		updates := len(p.updates)
		p.trackMu.Unlock()
		return updates == 1 && len(p.getSent()) == 1
	})

	delivery := e.trackStore.delivery(binding.Destination, "thread-1", completed.ID, "primary")
	if delivery == nil || !delivery.Terminal || delivery.Source != "external" || delivery.CardMessageID != "card-1" || delivery.NotificationState != "sent" {
		t.Fatalf("terminal delivery = %#v", delivery)
	}
	p.trackMu.Lock()
	if len(p.starts) != 1 || len(p.updates) != 1 {
		t.Fatalf("card lifecycle starts=%d updates=%d", len(p.starts), len(p.updates))
	}
	if len(p.options) != 2 {
		p.trackMu.Unlock()
		t.Fatalf("rich render count = %d, want progress start and progress finish", len(p.options))
	}
	lastOptions := p.options[len(p.options)-1]
	footerSendCount := len(p.footerSends)
	p.trackMu.Unlock()
	if lastOptions.Status != CardStatusDone || lastOptions.Variant != CardVariantMirror || lastOptions.Streaming {
		t.Fatalf("terminal mirror options = %#v", lastOptions)
	}
	if strings.Contains(lastOptions.Markdown, "external answer") || !strings.Contains(lastOptions.StatusFooter, "next message") {
		t.Fatalf("terminal progress card contains the final response: %#v", lastOptions)
	}
	if footerSendCount != 1 {
		t.Fatalf("structured terminal result sends = %d, want 1", footerSendCount)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "external answer") || strings.Contains(got, "task finished") {
		t.Fatalf("terminal result = %q", got)
	}

	agent.events <- Event{Type: EventResult, ThreadID: "thread-1", TurnID: completed.ID, Done: true}
	time.Sleep(100 * time.Millisecond)
	if got := len(p.getSent()); got != 1 {
		t.Fatalf("duplicate terminal event sent %d results", got)
	}
}

func TestConversationMirror_NotificationModesDoNotLoseOrDuplicateFinalResponse(t *testing.T) {
	tests := []struct {
		name           string
		notify         string
		status         ConversationTurnStatus
		separateResult bool
	}{
		{name: "never keeps completed response inline", notify: "never", status: ConversationTurnCompleted},
		{name: "on failure keeps successful response inline", notify: "on_failure", status: ConversationTurnCompleted},
		{name: "on failure sends failed response separately", notify: "on_failure", status: ConversationTurnFailed, separateResult: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
			p := newMirrorTestPlatform()
			e, binding, _ := startMirrorTest(t, agent, p)
			e.SetTrackCfg(TrackCfg{Enabled: true, DefaultEnabled: true, Notify: tc.notify, SharedWrite: "observer_only"})
			running := ConversationTurn{
				ID: "turn-mode", Status: ConversationTurnInProgress, StartedAt: time.Now(),
				Messages: []ConversationMessage{{Role: "user", Content: "mode prompt"}},
			}
			agent.setSnapshot(mirrorTestSnapshot("thread-1", running))
			agent.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: running.ID}
			waitMirrorTest(t, "mode progress card", func() bool {
				p.trackMu.Lock()
				defer p.trackMu.Unlock()
				return len(p.starts) == 1
			})

			terminal := running
			terminal.Status = tc.status
			terminal.CompletedAt = time.Now()
			terminal.Messages = append(terminal.Messages, ConversationMessage{Role: "assistant", Content: "mode answer", Phase: "final_answer"})
			agent.setSnapshot(&ConversationSnapshot{SessionID: "thread-1", ThreadState: "idle", Turns: []ConversationTurn{terminal}})
			agent.events <- Event{Type: EventResult, ThreadID: "thread-1", TurnID: terminal.ID, Done: true}
			waitMirrorTest(t, "mode terminal delivery", func() bool {
				delivery := e.trackStore.delivery(binding.Destination, "thread-1", terminal.ID, "primary")
				return delivery != nil && delivery.Terminal && delivery.NotificationState == "sent"
			})

			p.trackMu.Lock()
			options := append([]RichCardRenderOptions(nil), p.options...)
			p.trackMu.Unlock()
			sent := strings.Join(p.getSent(), "\n")
			if tc.separateResult {
				if len(options) != 2 || strings.Contains(options[len(options)-1].Markdown, "mode answer") {
					t.Fatalf("separate result render options = %#v", options)
				}
				if !strings.Contains(sent, "mode answer") {
					t.Fatalf("separate terminal result = %q", sent)
				}
				return
			}
			if len(options) != 2 || !strings.Contains(options[len(options)-1].Markdown, "mode answer") {
				t.Fatalf("inline terminal render options = %#v", options)
			}
			if sent != "" {
				t.Fatalf("inline terminal response also sent separately: %q", sent)
			}
		})
	}
}

func TestConversationMirror_RichCardReusesForegroundTurnPresentation(t *testing.T) {
	code := 0
	success := true
	turn := ConversationTurn{
		ID: "turn-external", Status: ConversationTurnCompleted,
		Messages: []ConversationMessage{
			{Role: "user", Content: "inspect the workspace"},
			{Role: "assistant", Content: "Ran command: ls", Phase: "commentary"},
			{Role: "assistant", Content: "The workspace is clean.", Phase: "final_answer"},
		},
		PresentationEvents: []Event{
			{Type: EventThinking, ItemID: "reasoning-1", Content: "Inspecting the workspace"},
			{Type: EventToolUse, ItemID: "tool-1", ToolName: "Bash", ToolInput: "ls"},
			{
				Type: EventToolResult, ItemID: "tool-1", ToolName: "Bash", ToolResult: "README.md",
				ToolStatus: "completed", ToolExitCode: &code, ToolSuccess: &success,
			},
			{Type: EventText, ItemID: "answer-1", Content: "The workspace is clean."},
		},
	}
	p := newMirrorTestPlatform()
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	markdown := e.renderTrackMarkdown(&ConversationSnapshot{}, turn)
	e.renderTrackPayload(p, &ConversationSnapshot{SessionID: "thread-1"}, turn, markdown, "mirror:chat:admin")

	p.trackMu.Lock()
	if len(p.options) != 1 {
		p.trackMu.Unlock()
		t.Fatalf("rendered options = %d, want 1", len(p.options))
	}
	options := p.options[0]
	p.trackMu.Unlock()

	if len(options.Steps) != 2 {
		t.Fatalf("steps = %#v, want one reasoning row and one merged tool row", options.Steps)
	}
	if options.Steps[0].Kind != ToolStepKindThinking || options.Steps[0].Summary != "Inspecting the workspace" {
		t.Fatalf("reasoning step = %#v", options.Steps[0])
	}
	tool := options.Steps[1]
	if tool.Kind != ToolStepKindTool || tool.ID != "tool-1" || tool.Summary != "ls" || tool.Result != "README.md" || !tool.Done {
		t.Fatalf("tool step = %#v", tool)
	}
	for _, want := range []string{"inspect the workspace", "The workspace is clean."} {
		if !strings.Contains(options.Markdown, want) {
			t.Fatalf("rich markdown %q does not contain %q", options.Markdown, want)
		}
	}
	for _, duplicate := range []string{"Ran command: ls", "Latest activity", "Turn status"} {
		if strings.Contains(options.Markdown, duplicate) {
			t.Fatalf("rich markdown duplicated progress text %q: %q", duplicate, options.Markdown)
		}
	}
	if len(options.ProgressItems) != 3 {
		t.Fatalf("progress items = %#v, want reasoning, tool call, and tool result rows", options.ProgressItems)
	}
	if options.ProgressItems[0].Kind != ProgressEntryThinking || options.ProgressItems[1].Kind != ProgressEntryToolUse || options.ProgressItems[2].Kind != ProgressEntryToolResult {
		t.Fatalf("progress item order = %#v", options.ProgressItems)
	}
	if options.ProgressItems[2].ExitCode == nil || *options.ProgressItems[2].ExitCode != 0 || options.Language != LangEnglish {
		t.Fatalf("tool result metadata = %#v, language = %q", options.ProgressItems[2], options.Language)
	}
	if options.ProgressCounts.Reasoning != 1 || options.ProgressCounts.Tools != 2 {
		t.Fatalf("mirror cumulative progress counts = %#v", options.ProgressCounts)
	}
}

func TestSetTrackCfg_ReenableResumesDefaultMirror(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.SetTrackCfg(TrackCfg{Enabled: false, DefaultEnabled: true, Notify: "on_finish", SharedWrite: "observer_only"})
	key := "mirror:chat:admin"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("thread-1", agent.Name())
	e.sessions.Save()
	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer e.Stop()
	waitMirrorTest(t, "disabled default binding", func() bool {
		return e.trackStore.binding("mirror:chat") != nil
	})
	if reads := agent.readCount(); reads != 0 {
		t.Fatalf("disabled mirror performed %d backend reads", reads)
	}

	e.SetTrackCfg(DefaultTrackCfg())
	waitMirrorTest(t, "mirror after config re-enable", func() bool { return agent.readCount() > 0 })
}

func TestResumeConversationMirrors_DoesNotGuessAmbiguousDestination(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	e.sessions.GetOrCreateActive("mirror:chat:user-a").SetAgentSessionID("thread-a", agent.Name())
	e.sessions.GetOrCreateActive("mirror:chat:user-b").SetAgentSessionID("thread-b", agent.Name())
	e.sessions.Save()

	e.resumeConversationMirrors(p)
	if binding := e.trackStore.binding("mirror:chat"); binding != nil {
		t.Fatalf("ambiguous default binding = %#v", binding)
	}
	if reads := agent.readCount(); reads != 0 {
		t.Fatalf("ambiguous destination performed %d backend reads", reads)
	}
}

func TestResumeConversationMirrors_SkipsWorkspaceMismatchedThreads(t *testing.T) {
	for _, tc := range []struct {
		name      string
		persisted bool
	}{
		{name: "default binding", persisted: false},
		{name: "persisted binding", persisted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
			p := newMirrorTestPlatform()
			e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
			key := "mirror:chat:admin"
			e.sessions.GetOrCreateActive(key).SetAgentSessionID("thread-1", agent.Name())
			e.sessions.Save()
			if tc.persisted {
				if _, err := e.bindConversationMirror(p, key, "thread-1"); err != nil {
					t.Fatalf("bindConversationMirror() error = %v", err)
				}
			}
			agent.readErr = errors.New("workspace mismatch")
			agent.watchErr = errors.New("workspace mismatch")

			e.resumeConversationMirrors(p)

			e.trackMu.Lock()
			running := len(e.conversationMirrors)
			e.trackMu.Unlock()
			if running != 0 || agent.watchCount() != 0 {
				t.Fatalf("workspace-mismatched mirror started: running=%d watches=%d", running, agent.watchCount())
			}
			binding := e.trackStore.binding("mirror:chat")
			if tc.persisted && binding == nil {
				t.Fatal("persisted binding was removed while skipping restore")
			}
			if !tc.persisted && binding != nil {
				t.Fatalf("unreadable default binding was persisted: %#v", binding)
			}
		})
	}
}

func TestConversationMirror_ForegroundMarkerDoesNotCreateDuplicateCard(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e, binding, key := startMirrorTest(t, agent, p)
	session := e.sessions.GetOrCreateActive(key)
	if err := e.prepareForegroundConversation(p, &Message{SessionKey: key, Platform: p.Name(), MessageID: "feishu-message"}, session, agent, e.sessions); err != nil {
		t.Fatalf("prepareForegroundConversation() error = %v", err)
	}
	running := ConversationTurn{
		ID: "turn-foreground", Status: ConversationTurnInProgress, StartedAt: time.Now(),
		Messages: []ConversationMessage{{Role: "user", Content: "from Feishu", ClientID: "feishu-message"}},
	}
	agent.setSnapshot(mirrorTestSnapshot("thread-1", running))
	agent.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: running.ID, ClientUserMessageID: "feishu-message"}
	waitMirrorTest(t, "foreground delivery claim", func() bool {
		return e.trackStore.delivery(binding.Destination, "thread-1", running.ID, "primary") != nil
	})
	p.trackMu.Lock()
	starts := len(p.starts)
	p.trackMu.Unlock()
	if starts != 0 {
		t.Fatalf("foreground turn created %d mirror cards", starts)
	}
	delivery := e.trackStore.delivery(binding.Destination, "thread-1", running.ID, "primary")
	if delivery.Source != "foreground" {
		t.Fatalf("foreground delivery source = %q", delivery.Source)
	}
	completed := running
	completed.Status = ConversationTurnCompleted
	completed.CompletedAt = time.Now()
	agent.setSnapshot(mirrorTestSnapshot("thread-1", completed))
	agent.events <- Event{Type: EventResult, ThreadID: "thread-1", TurnID: completed.ID, Done: true}
	waitMirrorTest(t, "foreground delivery terminal state", func() bool {
		terminal := e.trackStore.delivery(binding.Destination, "thread-1", completed.ID, "primary")
		return terminal != nil && terminal.Terminal
	})
	p.trackMu.Lock()
	starts = len(p.starts)
	p.trackMu.Unlock()
	if starts != 0 {
		t.Fatalf("completed foreground turn created %d mirror cards", starts)
	}
}

func TestPrepareForegroundConversation_GeneratesMarkerBeforeSend(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	msg := &Message{SessionKey: key, Platform: p.Name()}

	if err := e.prepareForegroundConversation(p, msg, session, agent, e.sessions); err != nil {
		t.Fatalf("prepareForegroundConversation() error = %v", err)
	}
	if !strings.HasPrefix(msg.ClientUserMessageID, "cc-connect-") {
		t.Fatalf("generated client marker = %q", msg.ClientUserMessageID)
	}
	reservation := e.trackStore.foregroundReservation(msg.ClientUserMessageID)
	if reservation == nil || reservation.ThreadID != "thread-1" || reservation.SourceMsgID != "" {
		t.Fatalf("foreground reservation = %#v", reservation)
	}
}

func TestPrepareQueuedForegroundConversation_ReservesQueuedMessage(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	state := &interactiveState{agent: agent}
	queued := queuedMessage{
		messageID: "queued-message", platform: p, replyCtx: "ctx",
		msgPlatform: p.Name(), msgSessionKey: key,
	}

	if err := e.prepareQueuedForegroundConversation(state, session, e.sessions, &queued); err != nil {
		t.Fatalf("prepareQueuedForegroundConversation() error = %v", err)
	}
	if queued.clientUserMessageID != "queued-message" {
		t.Fatalf("queued client marker = %q", queued.clientUserMessageID)
	}
	if reservation := e.trackStore.foregroundReservation("queued-message"); reservation == nil || reservation.ThreadID != "thread-1" {
		t.Fatalf("queued foreground reservation = %#v", reservation)
	}
}

func TestQueuedForegroundTurn_DoesNotCreateDuplicateMirrorCard(t *testing.T) {
	base := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	session := newMarkerQueueSession("thread-1")
	agent := &interactiveMirrorAgent{mirrorTestAgent: base, session: session}
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	key := "mirror:chat:admin"

	e.ReceiveMessage(p, &Message{
		SessionKey: key, Platform: p.Name(), MessageID: "message-a", UserID: "admin",
		Content: "first", ReplyCtx: "ctx-a",
	})
	select {
	case marker := <-session.sent:
		if marker != "message-a" {
			t.Fatalf("first Send marker = %q", marker)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first Send")
	}
	waitMirrorTest(t, "initial foreground mirror baseline", func() bool { return base.readCount() > 0 })

	e.ReceiveMessage(p, &Message{
		SessionKey: key, Platform: p.Name(), MessageID: "message-b", UserID: "admin",
		Content: "second", ReplyCtx: "ctx-b",
	})
	waitMirrorTest(t, "second message to enter the foreground queue", func() bool {
		e.interactiveMu.Lock()
		state := e.interactiveStates[key]
		e.interactiveMu.Unlock()
		if state == nil {
			return false
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		return len(state.pendingMessages) == 1
	})
	session.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: "turn-a"}
	session.events <- Event{Type: EventResult, ThreadID: "thread-1", TurnID: "turn-a", Content: "first done", Done: true}
	select {
	case marker := <-session.sent:
		if marker != "message-b" {
			t.Fatalf("queued Send marker = %q", marker)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for queued Send")
	}

	running := ConversationTurn{
		ID: "turn-b", Status: ConversationTurnInProgress, StartedAt: time.Now(),
		Messages: []ConversationMessage{{Role: "user", Content: "second", ClientID: "message-b"}},
	}
	base.setSnapshot(mirrorTestSnapshot("thread-1", running))
	base.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: running.ID}
	session.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: running.ID}
	session.events <- Event{Type: EventResult, ThreadID: "thread-1", TurnID: running.ID, Content: "second done", Done: true}

	waitMirrorTest(t, "queued foreground delivery", func() bool {
		delivery := e.trackStore.delivery("mirror:chat", "thread-1", running.ID, "primary")
		return delivery != nil && delivery.Source == "foreground"
	})
	p.trackMu.Lock()
	starts := len(p.starts)
	p.trackMu.Unlock()
	if starts != 0 {
		t.Fatalf("queued foreground turn created %d mirror cards", starts)
	}
	if reservation := e.trackStore.foregroundReservation("message-b"); reservation == nil || reservation.TurnID != running.ID {
		t.Fatalf("queued foreground reservation = %#v", reservation)
	}
	// The delivery is recorded before processInteractiveMessageWith finishes its
	// final session save and unlock. Wait for that public lifecycle boundary so
	// TempDir cleanup cannot race the background turn goroutine.
	foregroundSession := e.sessions.GetOrCreateActive(key)
	waitMirrorTest(t, "queued foreground turn cleanup", func() bool {
		return !foregroundSession.Busy()
	})
}

func TestPrepareForegroundConversation_FailsClosedWhenStateCannotPersist(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block mkdir"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(blockedParent, "sessions.json"), LangEnglish)
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	msg := &Message{SessionKey: key, Platform: p.Name(), MessageID: "feishu-message"}

	if err := e.prepareForegroundConversation(p, msg, session, agent, e.sessions); err == nil {
		t.Fatal("prepareForegroundConversation() error = nil, want persistence failure")
	}
	if msg.ClientUserMessageID != "" {
		t.Fatalf("client marker was assigned after failed binding: %q", msg.ClientUserMessageID)
	}
	if binding := e.trackStore.binding("mirror:chat"); binding != nil {
		t.Fatalf("failed binding leaked into memory: %#v", binding)
	}
}

func TestConversationMirror_RequiresRoundTripClientMarkers(t *testing.T) {
	base := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	agent := &mirrorNoMarkerAgent{mirrorTestAgent: base}
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	msg := &Message{SessionKey: key, Platform: p.Name(), MessageID: "feishu-message"}

	if err := e.prepareForegroundConversation(p, msg, session, agent, e.sessions); err != nil {
		t.Fatalf("unsupported mirror preparation returned error: %v", err)
	}
	if msg.ClientUserMessageID != "" || e.trackStore.binding("mirror:chat") != nil || base.readCount() != 0 {
		t.Fatalf("unsupported mirror mutated state: marker=%q binding=%#v reads=%d", msg.ClientUserMessageID, e.trackStore.binding("mirror:chat"), base.readCount())
	}
}

func TestConversationMirror_RequiresRuntimeCardSupport(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := &disabledMirrorTestPlatform{mirrorTestPlatform: newMirrorTestPlatform()}
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	msg := &Message{SessionKey: key, Platform: p.Name(), MessageID: "feishu-message"}

	if err := e.prepareForegroundConversation(p, msg, session, agent, e.sessions); err != nil {
		t.Fatalf("unsupported mirror preparation returned error: %v", err)
	}
	if msg.ClientUserMessageID != "" || e.trackStore.binding("mirror:chat") != nil || agent.readCount() != 0 {
		t.Fatalf("disabled card runtime mutated state: marker=%q binding=%#v reads=%d", msg.ClientUserMessageID, e.trackStore.binding("mirror:chat"), agent.readCount())
	}
}

func TestConversationMirror_ClosedObserverBacksOffBeforeReconnect(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	close(agent.events)
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	defer e.Stop()
	binding, err := e.bindConversationMirror(p, "mirror:chat:admin", "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	e.startConversationMirror(agent, e.sessions, p, binding)
	waitMirrorTest(t, "initial observer connection", func() bool { return agent.watchCount() >= 1 })

	time.Sleep(150 * time.Millisecond)
	if got := agent.watchCount(); got != 1 {
		t.Fatalf("observer reconnect count after immediate close = %d, want 1 before backoff", got)
	}
}

func TestConversationMirror_KnownExternalMarkerCannotConsumeForegroundReservation(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e := NewEngine("test", agent, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	key := "mirror:chat:admin"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	if !session.TryLock() {
		t.Fatal("TryLock() = false")
	}
	defer session.Unlock()
	binding, err := e.bindConversationMirror(p, key, "thread-1")
	if err != nil {
		t.Fatalf("bindConversationMirror() error = %v", err)
	}
	if err := e.trackStore.reserveForeground(trackForegroundReservation{
		ClientID: "cc-connect-known", Destination: binding.Destination, SessionKey: key,
		ThreadID: "thread-1", Generation: binding.Generation,
	}); err != nil {
		t.Fatalf("reserveForeground() error = %v", err)
	}
	turn := ConversationTurn{
		ID: "turn-external", Status: ConversationTurnInProgress, StartedAt: time.Now(),
		Messages: []ConversationMessage{{Role: "user", Content: "external prompt", ClientID: "another-client"}},
	}
	// Even a stale/racy turn association must lose to the authoritative,
	// non-empty client marker carried by the user message.
	if err := e.trackStore.confirmForegroundTurn("cc-connect-known", "thread-1", turn.ID); err != nil {
		t.Fatalf("confirmForegroundTurn() error = %v", err)
	}
	mirror := &conversationMirror{destination: binding.Destination, sessionKey: key, threadID: "thread-1", generation: binding.Generation, handles: make(map[string]any)}
	processed, err := e.deliverConversationTurn(e.ctx, mirror, binding, mirrorTestSnapshot("thread-1", turn), turn, e.sessions, p)
	if err != nil {
		t.Fatalf("deliverConversationTurn() error = %v", err)
	}
	if !processed {
		t.Fatal("known external marker was deferred behind a busy foreground turn")
	}
	delivery := e.trackStore.delivery(binding.Destination, "thread-1", turn.ID, "primary")
	if delivery == nil || delivery.Source != "external" {
		t.Fatalf("external delivery = %#v", delivery)
	}
}

func TestConversationMirror_HealthFailureIsVisibleOnExistingCard(t *testing.T) {
	p := newMirrorTestPlatform()
	e := NewEngine("test", newMirrorTestAgent(nil), []Platform{p}, "", LangChinese)
	binding, err := e.trackStore.bind("mirror:chat", "mirror:chat:admin", p.Name(), "thread-1")
	if err != nil {
		t.Fatalf("bind() error = %v", err)
	}
	delivery, _, err := e.trackStore.claimDelivery(binding, "turn-1", "primary", "external", "")
	if err != nil {
		t.Fatalf("claimDelivery() error = %v", err)
	}
	delivery, err = e.trackStore.setDeliveryHandle(delivery.Key, "card-1", "card-1")
	if err != nil {
		t.Fatalf("setDeliveryHandle() error = %v", err)
	}
	running := ConversationTurn{
		ID: "turn-1", Status: ConversationTurnInProgress,
		Messages: []ConversationMessage{{Role: "user", Content: "long task"}},
	}
	verifiedAt := time.Now().Add(-time.Minute)
	snapshot := mirrorTestSnapshot("thread-1", running)
	mirror := &conversationMirror{
		destination: binding.Destination, sessionKey: binding.SessionKey, threadID: binding.ThreadID,
		generation: binding.Generation, handles: map[string]any{delivery.Key: "card-1"},
		health: ProgressCardHealthVerified, lastVerifiedAt: verifiedAt, lastSnapshot: snapshot,
	}
	e.markConversationMirrorUnverifiedLocked(e.ctx, mirror, binding, p)
	p.trackMu.Lock()
	updates := append([]string(nil), p.updates...)
	p.trackMu.Unlock()
	if len(updates) != 1 || !strings.Contains(updates[0], "正在重新连接任务状态") {
		t.Fatalf("reconnecting external card updates = %#v", updates)
	}
	if strings.Contains(updates[0], e.i18n.T(MsgTrackInterruptButton)) {
		t.Fatalf("unverified external card retained interrupt action: %q", updates[0])
	}

	mirror.firstFailureAt = time.Now().Add(-cardHealthUnknownAfter)
	e.markConversationMirrorUnverifiedLocked(e.ctx, mirror, binding, p)
	p.trackMu.Lock()
	updates = append([]string(nil), p.updates...)
	p.trackMu.Unlock()
	if len(updates) != 2 || !strings.Contains(updates[1], "任务状态未知") {
		t.Fatalf("unknown external card updates = %#v", updates)
	}
}

func TestConversationMirror_TerminalCardFailureStillSendsResultOnce(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e, binding, _ := startMirrorTest(t, agent, p)
	running := ConversationTurn{
		ID: "turn-fails-update", Status: ConversationTurnInProgress, StartedAt: time.Now(),
		Messages: []ConversationMessage{{Role: "user", Content: "long task"}},
	}
	agent.setSnapshot(mirrorTestSnapshot("thread-1", running))
	agent.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: running.ID}
	waitMirrorTest(t, "initial card", func() bool {
		p.trackMu.Lock()
		defer p.trackMu.Unlock()
		return len(p.starts) == 1
	})
	p.trackMu.Lock()
	p.failUpdates = 3
	p.trackMu.Unlock()
	completed := running
	completed.Status = ConversationTurnCompleted
	completed.CompletedAt = time.Now()
	completed.Messages = append(completed.Messages, ConversationMessage{Role: "assistant", Content: "done", Phase: "final_answer"})
	agent.setSnapshot(&ConversationSnapshot{SessionID: "thread-1", ThreadState: "idle", Turns: []ConversationTurn{completed}})
	agent.events <- Event{Type: EventResult, ThreadID: "thread-1", TurnID: completed.ID, Done: true}
	waitMirrorTest(t, "card failure fallback result", func() bool { return len(p.getSent()) == 1 })
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "done") || !strings.Contains(got, "could not be updated") {
		t.Fatalf("fallback result = %q", got)
	}
	delivery := e.trackStore.delivery(binding.Destination, "thread-1", completed.ID, "primary")
	if delivery == nil || !delivery.Terminal || delivery.NotificationState != "sent" || delivery.LastError != "terminal_card_update_failed" {
		t.Fatalf("failed terminal delivery = %#v", delivery)
	}
	p.trackMu.Lock()
	attempts := p.updateAttempts
	p.trackMu.Unlock()
	if attempts != 3 {
		t.Fatalf("terminal update attempts = %d, want finite 3", attempts)
	}
}

func TestTrackedCardReplySteersExactTurnAndIndependentMessageIsHeldBack(t *testing.T) {
	running := ConversationTurn{
		ID: "turn-external", Status: ConversationTurnInProgress, StartedAt: time.Now(),
		Messages: []ConversationMessage{{Role: "user", Content: "external prompt"}},
	}
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e, _, key := startMirrorTest(t, agent, p)
	agent.setSnapshot(mirrorTestSnapshot("thread-1", running))
	agent.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: running.ID}
	waitMirrorTest(t, "tracked card", func() bool {
		p.trackMu.Lock()
		defer p.trackMu.Unlock()
		return len(p.starts) == 1
	})

	unauthorized := &Message{
		SessionKey: key, Platform: p.Name(), MessageID: "feishu-member", ReferencedMessageID: "card-1",
		UserID: "member", Content: "silently change it", ReplyCtx: "ctx",
	}
	if !e.handleTrackedConversationInput(p, unauthorized, unauthorized.Content, agent, e.sessions) {
		t.Fatal("unauthorized tracked card reply was not consumed")
	}
	agent.mu.Lock()
	unauthorizedSteers := len(agent.steers)
	agent.mu.Unlock()
	if unauthorizedSteers != 0 || !strings.Contains(strings.Join(p.getSent(), "\n"), "requires admin") {
		t.Fatalf("unauthorized card reply steers=%d response=%q", unauthorizedSteers, strings.Join(p.getSent(), "\n"))
	}
	p.clearSent()

	steerMessage := &Message{
		SessionKey: key, Platform: p.Name(), MessageID: "feishu-steer", ReferencedMessageID: "card-1",
		UserID: "admin", Content: "add this constraint", ReplyCtx: "ctx",
	}
	if !e.handleTrackedConversationInput(p, steerMessage, steerMessage.Content, agent, e.sessions) {
		t.Fatal("tracked card reply was not consumed")
	}
	agent.mu.Lock()
	steers := append([][4]string(nil), agent.steers...)
	agent.mu.Unlock()
	if len(steers) != 1 || steers[0] != [4]string{"thread-1", "turn-external", "add this constraint", "feishu-steer"} {
		t.Fatalf("exact steers = %#v", steers)
	}

	p.clearSent()
	independent := &Message{SessionKey: key, Platform: p.Name(), MessageID: "feishu-next", Content: "start another task", ReplyCtx: "ctx"}
	if !e.handleTrackedConversationInput(p, independent, independent.Content, agent, e.sessions) {
		t.Fatal("independent input during external turn was not held back")
	}
	agent.mu.Lock()
	steerCount := len(agent.steers)
	agent.mu.Unlock()
	if steerCount != 1 {
		t.Fatalf("independent input silently steered active turn: %#v", agent.steers)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "already running") || !strings.Contains(got, "not sent") {
		t.Fatalf("busy reply = %q", got)
	}
}

func TestTrackStateStore_PersistsPreferenceAndClaimsPrimaryOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "track.json")
	store := newTrackStateStore(path)
	binding, err := store.bind("mirror:chat", "mirror:chat:admin", "mirror", "thread-1")
	if err != nil {
		t.Fatalf("bind() error = %v", err)
	}
	if _, err := store.setOverride(binding.Destination, binding.SessionKey, binding.Platform, trackOverrideOff); err != nil {
		t.Fatalf("setOverride() error = %v", err)
	}

	var wg sync.WaitGroup
	created := make(chan bool, 8)
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, wasCreated, claimErr := store.claimDelivery(binding, "turn-1", "primary", "external", "")
			if claimErr != nil {
				t.Errorf("claimDelivery() error = %v", claimErr)
				return
			}
			created <- wasCreated
		}()
	}
	wg.Wait()
	close(created)
	createdCount := 0
	for value := range created {
		if value {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("primary delivery was created %d times", createdCount)
	}

	reloaded := newTrackStateStore(path)
	if got := reloaded.binding("mirror:chat"); got == nil || got.Override != trackOverrideOff || got.ThreadID != "thread-1" {
		t.Fatalf("reloaded binding = %#v", got)
	}
	if got := reloaded.delivery("mirror:chat", "thread-1", "turn-1", "primary"); got == nil || got.Source != "external" {
		t.Fatalf("reloaded delivery = %#v", got)
	}
	if _, err := reloaded.bind("mirror:other", "mirror:other:user", "mirror", "thread-1"); err == nil {
		t.Fatal("same backend thread was silently bound to a second destination")
	}
}

func TestTrackStateStore_PersistenceFailureRollsBackInMemoryPreference(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state-dir")
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	store := newTrackStateStore(statePath)
	if _, err := store.setOverride("mirror:chat", "mirror:chat:admin", "mirror", trackOverrideOff); err == nil {
		t.Fatal("setOverride() succeeded with a directory as its state file")
	}
	if got := store.binding("mirror:chat"); got != nil {
		t.Fatalf("failed persistence leaked an in-memory override: %#v", got)
	}
}

func TestCmdTrackOnOff_PersistsAndOnAdoptsOnlyActiveTurn(t *testing.T) {
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e, binding, key := startMirrorTest(t, agent, p)
	e.SetAdminFrom("admin")
	command := func(content string) {
		e.handleCommand(p, &Message{
			SessionKey: key, Platform: p.Name(), UserID: "admin", Content: content, ReplyCtx: "ctx",
		}, content)
	}

	command("/track off")
	waitMirrorTest(t, "mirror shutdown", func() bool {
		e.trackMu.Lock()
		defer e.trackMu.Unlock()
		return e.conversationMirrors[binding.Destination] == nil
	})
	if got := e.trackStore.binding(binding.Destination); got == nil || got.Override != trackOverrideOff || e.effectiveTrackEnabled(got) {
		t.Fatalf("disabled binding = %#v", got)
	}
	reloaded := newTrackStateStore(trackStatePath(e.sessions.StorePath()))
	if got := reloaded.binding(binding.Destination); got == nil || got.Override != trackOverrideOff {
		t.Fatalf("reloaded disabled binding = %#v", got)
	}

	completed := ConversationTurn{
		ID: "turn-while-off", Status: ConversationTurnCompleted, StartedAt: time.Now().Add(-time.Minute), CompletedAt: time.Now().Add(-time.Second),
		Messages: []ConversationMessage{{Role: "user", Content: "completed while off"}, {Role: "assistant", Content: "old answer", Phase: "final_answer"}},
	}
	running := ConversationTurn{
		ID: "turn-active", Status: ConversationTurnInProgress, StartedAt: time.Now(),
		Messages: []ConversationMessage{{Role: "user", Content: "active when enabled"}},
	}
	agent.setSnapshot(&ConversationSnapshot{SessionID: "thread-1", ThreadState: "active", Turns: []ConversationTurn{completed, running}})
	p.trackMu.Lock()
	p.starts = nil
	p.trackMu.Unlock()
	command("/track on")
	waitMirrorTest(t, "active turn adoption", func() bool {
		p.trackMu.Lock()
		defer p.trackMu.Unlock()
		return len(p.starts) == 1
	})
	p.trackMu.Lock()
	card := p.starts[0]
	p.trackMu.Unlock()
	if !strings.Contains(card, "active when enabled") || strings.Contains(card, "completed while off") {
		t.Fatalf("adopted card = %q", card)
	}
	if got := e.trackStore.binding(binding.Destination); got == nil || got.Override != trackOverrideOn || !e.effectiveTrackEnabled(got) {
		t.Fatalf("enabled binding = %#v", got)
	}
	e.SetTrackCfg(TrackCfg{Enabled: false, DefaultEnabled: true, Notify: "on_finish", SharedWrite: "observer_only"})
	if e.effectiveTrackEnabled(e.trackStore.binding(binding.Destination)) {
		t.Fatal("project-level track disable did not override destination on")
	}
	p.clearSent()
	command("/track on")
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "master switch") {
		t.Fatalf("project-disabled /track on reply = %q", got)
	}
}

func TestConversationMirror_RestartRestoresCardWithoutDuplicateCreate(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	agent := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	key := "mirror:chat:admin"

	e1 := NewEngine("test", agent, []Platform{p}, storePath, LangEnglish)
	session := e1.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("thread-1", agent.Name())
	e1.sessions.Save()
	binding, err := e1.bindConversationMirror(p, key, "thread-1")
	if err != nil {
		t.Fatalf("initial bind error = %v", err)
	}
	e1.startConversationMirror(agent, e1.sessions, p, binding)
	waitMirrorTest(t, "initial baseline", func() bool { return agent.readCount() > 0 })
	running := ConversationTurn{
		ID: "turn-recovered", Status: ConversationTurnInProgress, StartedAt: time.Now(),
		Messages: []ConversationMessage{{Role: "user", Content: "survive restart"}},
	}
	agent.setSnapshot(mirrorTestSnapshot("thread-1", running))
	agent.events <- Event{Type: EventTurnStarted, ThreadID: "thread-1", TurnID: running.ID}
	waitMirrorTest(t, "initial persisted card", func() bool {
		p.trackMu.Lock()
		defer p.trackMu.Unlock()
		return len(p.starts) == 1
	})
	e1.cancel()
	waitMirrorTest(t, "first mirror shutdown", func() bool {
		e1.trackMu.Lock()
		defer e1.trackMu.Unlock()
		return len(e1.conversationMirrors) == 0
	})

	completed := running
	completed.Status = ConversationTurnCompleted
	completed.CompletedAt = time.Now()
	completed.Messages = append(completed.Messages, ConversationMessage{Role: "assistant", Content: "recovered answer", Phase: "final_answer"})
	agent.setSnapshot(&ConversationSnapshot{SessionID: "thread-1", ThreadState: "idle", Turns: []ConversationTurn{completed}})
	e2 := NewEngine("test", agent, []Platform{p}, storePath, LangEnglish)
	t.Cleanup(func() {
		e2.cancel()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			e2.trackMu.Lock()
			runningMirrors := len(e2.conversationMirrors)
			e2.trackMu.Unlock()
			if runningMirrors == 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	e2.resumeConversationMirrors(p)
	waitMirrorTest(t, "recovered terminal delivery", func() bool {
		p.trackMu.Lock()
		updates := len(p.updates)
		starts := len(p.starts)
		p.trackMu.Unlock()
		return starts == 1 && updates == 1 && len(p.getSent()) == 1
	})
	delivery := e2.trackStore.delivery(binding.Destination, "thread-1", running.ID, "primary")
	if delivery == nil || delivery.CardHandle != "card-1" || delivery.NotificationState != "sent" || !delivery.Terminal {
		t.Fatalf("recovered delivery = %#v", delivery)
	}
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "recovered answer") || strings.Contains(got, "task finished") {
		t.Fatalf("recovered terminal result = %q", got)
	}
}

type silentUnsolicitedSession struct {
	*controllableAgentSession
}

func (*silentUnsolicitedSession) RelayUnsolicitedEvents() bool { return false }

func TestUnsolicitedReader_SilentPolicyDoesNotEchoOrStore(t *testing.T) {
	p := &stubPlatformEngine{n: "feishu"}
	sess := &silentUnsolicitedSession{controllableAgentSession: newControllableSession("thread-1")}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	defer e.Stop()
	e.SetAgentSessionIdleTimeout(time.Hour)
	session := e.sessions.GetOrCreateActive("feishu:chat:user")
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx"}
	e.interactiveMu.Lock()
	e.interactiveStates["feishu:chat:user"] = state
	e.interactiveMu.Unlock()
	e.startUnsolicitedReader(state, session, e.sessions, "feishu:chat:user", "")
	defer e.stopUnsolicitedReader(state)

	sess.events <- Event{Type: EventText, Content: "external commentary"}
	sess.events <- Event{Type: EventResult, Content: "external final", Done: true}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state.mu.Lock()
		processed := state.agentSessionIdleToken != 0
		state.mu.Unlock()
		if processed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.getSent(); len(got) != 0 {
		t.Fatalf("silent unsolicited turn was echoed: %v", got)
	}
	if got := session.GetHistory(10); len(got) != 0 {
		t.Fatalf("silent unsolicited turn was stored locally: %#v", got)
	}
}
