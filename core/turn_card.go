package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const turnCardInterruptActionPrefix = "turn:interrupt:"

const (
	nativeTurnCardHealthInterval = 15 * time.Second
	cardHealthUnknownAfter       = time.Minute
)

type activeTurnCard struct {
	identity     turnCardState
	provider     ConversationProvider
	canInterrupt bool
	canSteer     bool
	registered   bool
}

type nativeTurnCardMonitor struct {
	token    string
	threadID string
	turnID   string
	provider ConversationProvider
	writer   *compactProgressWriter
	cancel   context.CancelFunc
	checkMu  sync.Mutex
	lastOK   time.Time
	failedAt time.Time
}

func (e *Engine) beginActiveTurnCard(state *interactiveState, p Platform, sessionKey, interactiveKey, threadID, turnID string, agent Agent) *activeTurnCard {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if state == nil || p == nil || threadID == "" || turnID == "" {
		return nil
	}

	state.mu.Lock()
	if state.currentThreadID != threadID || state.currentTurnID != turnID {
		state.currentTurnGeneration++
		if state.currentTurnGeneration == 0 {
			state.currentTurnGeneration++
		}
	}
	state.currentThreadID = threadID
	state.currentTurnID = turnID
	generation := state.currentTurnGeneration
	state.mu.Unlock()

	_, canRead := agent.(ConversationProvider)
	_, hasInterrupt := agent.(ConversationTurnController)
	_, hasSteer := agent.(ConversationTurnSteerer)
	exactCards, supportsExactCards := p.(ExactTurnCardSupport)
	if !supportsExactCards || !exactCards.SupportsExactTurnCards() {
		return nil
	}
	canInterrupt := canRead && hasInterrupt
	canSteer := canRead && hasSteer
	if !canInterrupt && !canSteer {
		return nil
	}
	if e.turnCards == nil {
		return nil
	}
	if _, ok := p.(PreviewHandleIdentifier); !ok {
		return nil
	}
	token, err := newTurnCardToken()
	if err != nil {
		slog.Warn("turn card: controls disabled because token generation failed", "platform", p.Name(), "error", err)
		return nil
	}
	card := &activeTurnCard{
		identity: turnCardState{
			Token: token, Platform: p.Name(), SessionKey: sessionKey, InteractiveKey: interactiveKey,
			ThreadID: threadID, TurnID: turnID, Generation: generation, Status: string(ConversationTurnInProgress),
		},
		canInterrupt: canInterrupt,
		canSteer:     canSteer,
	}
	if destination, err := mirrorDestinationKey(p, sessionKey); err == nil {
		card.identity.Destination = destination
	}
	if provider, ok := agent.(ConversationProvider); ok {
		card.provider = provider
	}
	return card
}

func (e *Engine) activateTurnCard(p Platform, writer *compactProgressWriter, card *activeTurnCard, handle any) {
	if p == nil || writer == nil || card == nil || card.registered || handle == nil {
		return
	}
	messageID, err := previewMessageID(p, handle)
	if err != nil {
		slog.Warn("turn card: cannot bind progress card identity", "platform", p.Name(), "error", err)
		return
	}
	card.identity.CardMessageID = messageID
	if err := e.turnCards.register(card.identity); err != nil {
		slog.Error("turn card: persist exact card identity failed; controls remain hidden", "platform", p.Name(), "error", err)
		return
	}
	card.registered = true

	var hint string
	if card.canSteer {
		hint = e.i18n.T(MsgTurnCardReplyHint)
	}
	var buttons []CardButton
	if card.canInterrupt {
		buttons = []CardButton{{
			Text:  e.i18n.T(MsgTrackInterruptButton),
			Type:  "danger",
			Value: turnCardInterruptActionPrefix + card.identity.Token,
			Extra: map[string]string{"session_key": card.identity.SessionKey},
		}}
	}
	if !writer.SetControls(hint, buttons) {
		slog.Warn("turn card: failed to render exact turn controls", "platform", p.Name(), "turn_id", card.identity.TurnID)
	}
	if card.provider != nil {
		e.startNativeTurnCardMonitor(card, writer)
	}
}

func (e *Engine) startNativeTurnCardMonitor(card *activeTurnCard, writer *compactProgressWriter) {
	if card == nil || writer == nil || card.provider == nil || card.identity.Token == "" {
		return
	}
	ctx, cancel := context.WithCancel(e.ctx)
	monitor := &nativeTurnCardMonitor{
		token: card.identity.Token, threadID: card.identity.ThreadID, turnID: card.identity.TurnID,
		provider: card.provider, writer: writer, cancel: cancel,
	}
	writer.EnableHealthMonitoring()
	e.turnCardMonitorMu.Lock()
	if previous := e.turnCardMonitors[monitor.token]; previous != nil {
		previous.cancel()
	}
	e.turnCardMonitors[monitor.token] = monitor
	e.turnCardMonitorMu.Unlock()
	go e.runNativeTurnCardMonitor(ctx, monitor)
}

func (e *Engine) runNativeTurnCardMonitor(ctx context.Context, monitor *nativeTurnCardMonitor) {
	defer func() {
		e.turnCardMonitorMu.Lock()
		if e.turnCardMonitors[monitor.token] == monitor {
			delete(e.turnCardMonitors, monitor.token)
		}
		e.turnCardMonitorMu.Unlock()
		monitor.writer.DisableHealthMonitoring()
	}()
	e.checkNativeTurnCardMonitor(ctx, monitor, nil)
	ticker := time.NewTicker(nativeTurnCardHealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.checkNativeTurnCardMonitor(ctx, monitor, nil)
		}
	}
}

func (e *Engine) nativeTurnCardMonitor(token string) *nativeTurnCardMonitor {
	e.turnCardMonitorMu.Lock()
	defer e.turnCardMonitorMu.Unlock()
	return e.turnCardMonitors[strings.TrimSpace(token)]
}

func (e *Engine) stopNativeTurnCardMonitor(token string) {
	token = strings.TrimSpace(token)
	e.turnCardMonitorMu.Lock()
	monitor := e.turnCardMonitors[token]
	if monitor != nil {
		delete(e.turnCardMonitors, token)
	}
	e.turnCardMonitorMu.Unlock()
	if monitor != nil {
		monitor.writer.DisableHealthMonitoring()
		monitor.cancel()
	}
}

func (e *Engine) checkNativeTurnCardMonitor(ctx context.Context, monitor *nativeTurnCardMonitor, snapshot *ConversationSnapshot) (ProgressCardHealth, ConversationTurnStatus, error) {
	if monitor == nil {
		return ProgressCardHealthUnknown, ConversationTurnUnknown, fmt.Errorf("native turn card monitor not found")
	}
	monitor.checkMu.Lock()
	defer monitor.checkMu.Unlock()
	now := time.Now()
	var err error
	if snapshot == nil {
		readCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		snapshot, err = monitor.provider.GetConversation(readCtx, monitor.threadID, 8)
		cancel()
	}
	if err == nil && (snapshot == nil || snapshot.SessionID != monitor.threadID) {
		err = fmt.Errorf("unexpected conversation snapshot")
	}
	var turn ConversationTurn
	var found bool
	if err == nil {
		turn, found = conversationTurnByID(snapshot, monitor.turnID)
		if !found {
			err = fmt.Errorf("turn not found in conversation snapshot")
		}
	}
	if err != nil {
		if monitor.failedAt.IsZero() {
			monitor.failedAt = now
		}
		health := ProgressCardHealthReconnecting
		if now.Sub(monitor.failedAt) >= cardHealthUnknownAfter {
			health = ProgressCardHealthUnknown
		}
		monitor.writer.SetHealth(health, monitor.lastOK)
		return health, ConversationTurnUnknown, err
	}

	verifiedAt := snapshot.RetrievedAt
	if verifiedAt.IsZero() {
		verifiedAt = now
	}
	monitor.lastOK = verifiedAt
	monitor.failedAt = time.Time{}
	if conversationTurnTerminal(turn.Status) {
		state := ProgressCardStateCompleted
		switch turn.Status {
		case ConversationTurnFailed:
			state = ProgressCardStateFailed
		case ConversationTurnInterrupted:
			state = ProgressCardStateInterrupted
		}
		monitor.writer.SetHealth(ProgressCardHealthVerified, verifiedAt)
		monitor.writer.DisableHealthMonitoring()
		monitor.writer.Finalize(state)
		if err := e.turnCards.markTerminal(monitor.token, string(turn.Status)); err != nil {
			slog.Warn("turn card: persist monitored terminal state failed", "turn_id", monitor.turnID, "error", err)
		}
		e.stopNativeTurnCardMonitor(monitor.token)
		return ProgressCardHealthVerified, turn.Status, nil
	}
	if turn.Status == ConversationTurnUnknown {
		monitor.writer.SetHealth(ProgressCardHealthUnknown, verifiedAt)
		return ProgressCardHealthUnknown, turn.Status, nil
	}
	monitor.writer.SetHealth(ProgressCardHealthVerified, verifiedAt)
	return ProgressCardHealthVerified, turn.Status, nil
}

func progressStateConversationStatus(state ProgressCardState) string {
	switch state {
	case ProgressCardStateCompleted:
		return string(ConversationTurnCompleted)
	case ProgressCardStateInterrupted:
		return string(ConversationTurnInterrupted)
	case ProgressCardStateFailed:
		return string(ConversationTurnFailed)
	default:
		return string(ConversationTurnUnknown)
	}
}

func (e *Engine) finishActiveTurnCard(state *interactiveState, writer *compactProgressWriter, card *activeTurnCard, terminal ProgressCardState) {
	if terminal == "" {
		terminal = ProgressCardStateCompleted
	}
	if card != nil && card.registered && e.turnCards != nil {
		e.stopNativeTurnCardMonitor(card.identity.Token)
		if err := e.turnCards.markTerminal(card.identity.Token, progressStateConversationStatus(terminal)); err != nil {
			slog.Error("turn card: persist terminal identity failed", "turn_id", card.identity.TurnID, "error", err)
		}
	}
	e.clearActiveTurnIdentity(state, card)
	if writer != nil {
		writer.Finalize(terminal)
	}
}

func (e *Engine) expireActiveTurnCard(state *interactiveState, card *activeTurnCard) {
	if card != nil {
		if monitor := e.nativeTurnCardMonitor(card.identity.Token); monitor != nil {
			monitor.writer.SetControls("", nil)
		}
	}
	if card != nil && card.registered && e.turnCards != nil {
		if err := e.turnCards.markTerminal(card.identity.Token, "stale"); err != nil {
			slog.Error("turn card: persist stale identity failed", "turn_id", card.identity.TurnID, "error", err)
		}
	}
	e.clearActiveTurnIdentity(state, card)
}

func (e *Engine) clearActiveTurnIdentity(state *interactiveState, card *activeTurnCard) {
	if state == nil {
		return
	}
	state.mu.Lock()
	if card == nil || (state.currentThreadID == card.identity.ThreadID &&
		state.currentTurnID == card.identity.TurnID && state.currentTurnGeneration == card.identity.Generation) {
		state.currentThreadID = ""
		state.currentTurnID = ""
	}
	state.mu.Unlock()
}

func progressCardStateFromResult(event Event) ProgressCardState {
	status := ""
	if event.Metadata != nil {
		if value, ok := event.Metadata["turn_status"]; ok && value != nil {
			status = normalizedTrackStatus(fmt.Sprint(value))
		}
	}
	switch status {
	case "failed":
		return ProgressCardStateFailed
	case "interrupted":
		return ProgressCardStateInterrupted
	default:
		return ProgressCardStateCompleted
	}
}

func (e *Engine) liveTurnCardState(card *turnCardState, agent Agent, sessions *SessionManager, interactiveKey string) (*Session, bool) {
	if card == nil || card.Terminal || card.InterruptRequested || agent == nil || sessions == nil || card.InteractiveKey != interactiveKey {
		return nil, false
	}
	activeID := sessions.ActiveSessionID(card.SessionKey)
	if activeID == "" {
		return nil, false
	}
	session := sessions.FindByID(activeID)
	if session == nil || session.GetAgentSessionID() != card.ThreadID || !session.Busy() {
		return nil, false
	}

	e.interactiveMu.Lock()
	state := e.interactiveStates[interactiveKey]
	e.interactiveMu.Unlock()
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	valid := !state.stopped && state.agentSession != nil &&
		state.currentThreadID == card.ThreadID && state.currentTurnID == card.TurnID &&
		state.currentTurnGeneration == card.Generation
	state.mu.Unlock()
	return session, valid
}

func authoritativeTurnStillRunning(ctx context.Context, provider ConversationProvider, threadID, turnID string) (bool, error) {
	snapshot, err := provider.GetConversation(ctx, threadID, 8)
	if err != nil {
		return false, err
	}
	if snapshot == nil || snapshot.SessionID != threadID {
		return false, fmt.Errorf("unexpected conversation snapshot")
	}
	turn, ok := conversationTurnByID(snapshot, turnID)
	return ok && turn.Status == ConversationTurnInProgress, nil
}

func (e *Engine) handleTurnCardAction(p Platform, msg *Message, content string, agent Agent, sessions *SessionManager, interactiveKey string) bool {
	if msg == nil || !msg.IsCardAction || !strings.HasPrefix(content, "turn:") {
		return false
	}
	if e.turnCards == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTurnCardInterruptStale))
		return true
	}
	token := strings.TrimSpace(strings.TrimPrefix(content, turnCardInterruptActionPrefix))
	card := e.turnCards.byToken(token)
	if !strings.HasPrefix(content, turnCardInterruptActionPrefix) || card == nil ||
		!strings.EqualFold(card.Platform, p.Name()) || card.SessionKey != msg.SessionKey ||
		card.CardMessageID == "" || card.CardMessageID != msg.ReferencedMessageID {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTurnCardInterruptStale))
		return true
	}
	provider, canRead := agent.(ConversationProvider)
	controller, canInterrupt := agent.(ConversationTurnController)
	if !canRead || !canInterrupt {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTurnCardInterruptStale))
		return true
	}
	if _, valid := e.liveTurnCardState(card, agent, sessions, interactiveKey); !valid {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTurnCardInterruptStale))
		return true
	}
	readCtx, readCancel := context.WithTimeout(e.ctx, 4*time.Second)
	running, err := authoritativeTurnStillRunning(readCtx, provider, card.ThreadID, card.TurnID)
	readCancel()
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTurnCardInterruptFailed, err))
		return true
	}
	if !running {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTurnCardInterruptStale))
		return true
	}
	claimed, err := e.turnCards.claimInterrupt(card.Token)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTurnCardInterruptFailed, err))
		return true
	}
	if !claimed {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTurnCardInterruptStale))
		return true
	}
	interruptCtx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	err = controller.InterruptConversationTurn(interruptCtx, card.ThreadID, card.TurnID)
	cancel()
	if err != nil {
		if releaseErr := e.turnCards.releaseInterrupt(card.Token); releaseErr != nil {
			slog.Warn("turn card: release failed interrupt claim", "turn_id", card.TurnID, "error", releaseErr)
		}
		slog.Error("turn card: exact interrupt failed", "thread_id", card.ThreadID, "turn_id", card.TurnID, "error", err)
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTurnCardInterruptFailed, err))
	}
	// Success is deliberately silent. The authoritative terminal event updates
	// the original card and removes its controls.
	return true
}

func (e *Engine) handleTurnCardReply(p Platform, msg *Message, directContent string, agent Agent, sessions *SessionManager, interactiveKey string) bool {
	if msg == nil || strings.TrimSpace(msg.ReferencedMessageID) == "" {
		return false
	}
	if e.turnCards == nil {
		return false
	}
	card := e.turnCards.byMessage(p.Name(), "", msg.ReferencedMessageID)
	if card == nil {
		return false
	}
	if card.SessionKey != msg.SessionKey {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSteerStale))
		return true
	}
	provider, canRead := agent.(ConversationProvider)
	steerer, canSteer := agent.(ConversationTurnSteerer)
	if !canRead || !canSteer {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSteerStale))
		return true
	}
	session, valid := e.liveTurnCardState(card, agent, sessions, interactiveKey)
	if !valid || (strings.TrimSpace(directContent) == "" && len(msg.Images) == 0 && len(msg.Files) == 0) {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSteerStale))
		return true
	}
	readCtx, readCancel := context.WithTimeout(e.ctx, 4*time.Second)
	running, err := authoritativeTurnStillRunning(readCtx, provider, card.ThreadID, card.TurnID)
	readCancel()
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackSteerFailed, err))
		return true
	}
	if !running {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSteerStale))
		return true
	}
	steerCtx, steerCancel := context.WithTimeout(e.ctx, 5*time.Second)
	err = steerer.SteerConversationTurn(
		steerCtx, card.ThreadID, card.TurnID, directContent, msg.MessageID, msg.Images, msg.Files,
	)
	steerCancel()
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackSteerFailed, err))
		return true
	}
	runMessageAccepted(msg)
	session.TouchUserActivity()
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSteerAccepted))
	return true
}
