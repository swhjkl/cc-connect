package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const turnCardInterruptActionPrefix = "turn:interrupt:"

type activeTurnCard struct {
	identity     turnCardState
	canInterrupt bool
	canSteer     bool
	registered   bool
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
	return &activeTurnCard{
		identity: turnCardState{
			Token: token, Platform: p.Name(), SessionKey: sessionKey, InteractiveKey: interactiveKey,
			ThreadID: threadID, TurnID: turnID, Generation: generation, Status: string(ConversationTurnInProgress),
		},
		canInterrupt: canInterrupt,
		canSteer:     canSteer,
	}
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
