package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	trackSectionMaxBytes   = 6_000
	trackReconcileMaxTurns = 256
)

type conversationTracker struct {
	cancel    context.CancelFunc
	sessionID string
	turnID    string
}

type conversationMirror struct {
	cancel      context.CancelFunc
	destination string
	sessionKey  string
	threadID    string
	generation  uint64
	wake        chan struct{}

	mu      sync.Mutex
	handles map[string]any
}

func (m *conversationMirror) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) historyEntries(ctx context.Context, agent Agent, session *Session, limit int) ([]HistoryEntry, error) {
	if provider, ok := agent.(ConversationProvider); ok {
		sessionID := session.GetAgentSessionID()
		if sessionID == "" {
			return nil, nil
		}
		snapshot, err := provider.GetConversation(ctx, sessionID, limit)
		if err != nil {
			return nil, err
		}
		return conversationHistoryEntries(snapshot), nil
	}

	entries := session.GetHistory(limit)
	sessionID := session.GetAgentSessionID()
	if len(entries) == 0 && sessionID != "" {
		if provider, ok := agent.(HistoryProvider); ok {
			backendEntries, err := provider.GetSessionHistory(ctx, sessionID, limit)
			if err != nil {
				return nil, err
			}
			entries = backendEntries
		}
	}
	return entries, nil
}

func conversationHistoryEntries(snapshot *ConversationSnapshot) []HistoryEntry {
	if snapshot == nil {
		return nil
	}
	var entries []HistoryEntry
	for _, turn := range snapshot.Turns {
		if !conversationTurnTerminal(turn.Status) {
			continue
		}
		prompt := conversationPrompt(turn)
		if prompt != "" {
			entries = append(entries, HistoryEntry{Role: "user", Content: prompt, Timestamp: turn.StartedAt})
		}
		response := conversationFinalAnswer(turn)
		if response != "" {
			entries = append(entries, HistoryEntry{Role: "assistant", Content: response, Timestamp: turn.CompletedAt})
		}
	}
	return entries
}

func conversationPrompt(turn ConversationTurn) string {
	var parts []string
	for _, message := range turn.Messages {
		if message.Role == "user" && strings.TrimSpace(message.Content) != "" {
			parts = append(parts, strings.TrimSpace(message.Content))
		}
	}
	return strings.Join(parts, "\n\n")
}

func conversationFinalAnswer(turn ConversationTurn) string {
	var final string
	var legacy string
	for _, message := range turn.Messages {
		if message.Role != "assistant" || strings.TrimSpace(message.Content) == "" {
			continue
		}
		phase := strings.ToLower(strings.TrimSpace(message.Phase))
		if phase == "final_answer" || phase == "finalanswer" {
			final = strings.TrimSpace(message.Content)
		}
		if phase == "" {
			legacy = strings.TrimSpace(message.Content)
		}
	}
	if final != "" {
		return final
	}
	return legacy
}

func conversationLiveResponse(turn ConversationTurn) string {
	if final := conversationFinalAnswer(turn); final != "" {
		return final
	}
	var commentary []string
	for _, message := range turn.Messages {
		if message.Role != "assistant" || strings.TrimSpace(message.Content) == "" {
			continue
		}
		phase := strings.ToLower(strings.TrimSpace(message.Phase))
		if phase == "commentary" || phase == "" {
			commentary = append(commentary, strings.TrimSpace(message.Content))
		}
	}
	return strings.Join(commentary, "\n\n")
}

func conversationTurnTerminal(status ConversationTurnStatus) bool {
	switch status {
	case ConversationTurnCompleted, ConversationTurnFailed, ConversationTurnInterrupted:
		return true
	default:
		return false
	}
}

func historyTimestamp(timestamp time.Time) string {
	if timestamp.IsZero() {
		return "--:--:--"
	}
	return timestamp.Local().Format("15:04:05")
}

func (e *Engine) effectiveTrackEnabled(binding *trackBindingState) bool {
	if binding == nil {
		return false
	}
	e.trackMu.Lock()
	cfg := e.trackCfg
	e.trackMu.Unlock()
	if !cfg.Enabled {
		return false
	}
	switch binding.Override {
	case trackOverrideOn:
		return true
	case trackOverrideOff:
		return false
	default:
		return cfg.DefaultEnabled
	}
}

// supportsConversationMirror requires the identity and recovery capabilities
// that make continuous mirroring safe. In particular, the client marker is
// what prevents a turn started through cc-connect from being echoed back as an
// external turn.
func supportsConversationMirror(agent Agent, p Platform) bool {
	if agent == nil || p == nil {
		return false
	}
	if _, ok := agent.(ConversationProvider); !ok {
		return false
	}
	if _, ok := agent.(ConversationEventSource); !ok {
		return false
	}
	marker, ok := agent.(ConversationClientMarkerSupport)
	if !ok || !marker.SupportsConversationClientMarker() {
		return false
	}
	if _, ok := p.(ConversationMirrorDestination); !ok {
		return false
	}
	if supporter, ok := p.(ConversationMirrorSupporter); ok && !supporter.SupportsConversationMirror() {
		return false
	}
	if _, ok := p.(ReplyContextReconstructor); !ok {
		return false
	}
	if _, ok := p.(MessageUpdater); !ok {
		return false
	}
	if _, ok := p.(IdempotentPreviewStarter); !ok {
		return false
	}
	if _, ok := p.(PreviewHandleCodec); !ok {
		return false
	}
	if _, ok := p.(PreviewHandleIdentifier); !ok {
		return false
	}
	return true
}

func mirrorDestinationKey(p Platform, sessionKey string) (string, error) {
	if resolver, ok := p.(ConversationMirrorDestination); ok {
		return resolver.MirrorDestinationKey(sessionKey)
	}
	if strings.TrimSpace(sessionKey) == "" {
		return "", fmt.Errorf("track: session key is empty")
	}
	return sessionKey, nil
}

func (e *Engine) bindConversationMirror(p Platform, sessionKey, threadID string) (*trackBindingState, error) {
	destination, err := mirrorDestinationKey(p, sessionKey)
	if err != nil {
		return nil, err
	}
	return e.trackStore.bind(destination, sessionKey, p.Name(), threadID)
}

func (e *Engine) detachConversationMirror(p Platform, sessionKey string) error {
	destination, err := mirrorDestinationKey(p, sessionKey)
	if err != nil {
		return err
	}
	binding := e.trackStore.binding(destination)
	if binding == nil || (binding.SessionKey != "" && binding.SessionKey != sessionKey) {
		return nil
	}
	if _, err := e.trackStore.unbind(destination, sessionKey, p.Name()); err != nil {
		return err
	}
	e.stopConversationMirror(destination)
	return nil
}

func (e *Engine) rebindConversationMirror(p Platform, sessionKey, threadID string, agent Agent) (*trackBindingState, error) {
	if !supportsConversationMirror(agent, p) {
		return nil, nil
	}
	destination, err := mirrorDestinationKey(p, sessionKey)
	if err != nil {
		return nil, err
	}
	if existing := e.trackStore.binding(destination); existing != nil && existing.SessionKey != "" && existing.SessionKey != sessionKey {
		return nil, nil
	}
	binding, err := e.trackStore.bind(destination, sessionKey, p.Name(), threadID)
	if err != nil {
		return nil, err
	}
	e.stopConversationMirror(destination)
	return binding, nil
}

// prepareForegroundConversation reserves the platform message marker before
// the backend turn/start request, then starts the passive observer. The
// persisted reservation lets observer/snapshot reconciliation classify the
// same turn as foreground even when event and RPC response order is inverted.
func (e *Engine) prepareForegroundConversation(p Platform, msg *Message, session *Session, agent Agent, sessions *SessionManager) error {
	if msg == nil || session == nil || agent == nil {
		return nil
	}
	if !supportsConversationMirror(agent, p) {
		return nil
	}
	threadID := session.GetAgentSessionID()
	if threadID == "" {
		return nil
	}
	destination, err := mirrorDestinationKey(p, msg.SessionKey)
	if err != nil {
		return err
	}
	existing := e.trackStore.binding(destination)
	if existing != nil && existing.SessionKey != "" && existing.SessionKey != msg.SessionKey {
		// A proactive destination can belong to only one logical session. Do
		// not block ordinary chat from another session that happens to share it.
		return nil
	}
	effective := existing
	if effective == nil {
		effective = &trackBindingState{Destination: destination, SessionKey: msg.SessionKey, Platform: p.Name(), ThreadID: threadID}
	}
	if !e.effectiveTrackEnabled(effective) {
		return nil
	}
	binding, err := e.trackStore.bind(destination, msg.SessionKey, p.Name(), threadID)
	if err != nil {
		return err
	}
	marker := strings.TrimSpace(msg.ClientUserMessageID)
	if marker == "" {
		marker = strings.TrimSpace(msg.MessageID)
	}
	if marker == "" {
		sequence := e.trackClientSeq.Add(1)
		seed := fmt.Sprintf("%s\x00%s\x00%d\x00%d", e.name, session.ID, time.Now().UnixNano(), sequence)
		marker = "cc-connect-" + trackIdempotencyKey(seed, "foreground")
	}
	msg.ClientUserMessageID = marker
	if err := e.trackStore.reserveForeground(trackForegroundReservation{
		ClientID: marker, Destination: binding.Destination, SessionKey: msg.SessionKey,
		ThreadID: threadID, Generation: binding.Generation, SourceMsgID: strings.TrimSpace(msg.MessageID),
	}); err != nil {
		return err
	}
	e.startConversationMirror(agent, sessions, p, binding)
	return nil
}

func (e *Engine) startConversationMirror(agent Agent, sessions *SessionManager, p Platform, binding *trackBindingState) {
	if binding == nil || !e.effectiveTrackEnabled(binding) {
		return
	}
	if !supportsConversationMirror(agent, p) {
		return
	}

	e.trackMu.Lock()
	if current := e.conversationMirrors[binding.Destination]; current != nil {
		if current.threadID == binding.ThreadID && current.generation == binding.Generation {
			e.trackMu.Unlock()
			current.signal()
			return
		}
		delete(e.conversationMirrors, binding.Destination)
		current.cancel()
	}
	ctx, cancel := context.WithCancel(e.ctx)
	mirror := &conversationMirror{
		cancel: cancel, destination: binding.Destination, sessionKey: binding.SessionKey,
		threadID: binding.ThreadID, generation: binding.Generation, wake: make(chan struct{}, 1),
		handles: make(map[string]any),
	}
	e.conversationMirrors[binding.Destination] = mirror
	e.trackMu.Unlock()
	go e.runConversationMirror(ctx, mirror, agent, sessions, p)
}

func (e *Engine) stopConversationMirror(destination string) {
	e.trackMu.Lock()
	mirror := e.conversationMirrors[destination]
	if mirror != nil {
		delete(e.conversationMirrors, destination)
	}
	e.trackMu.Unlock()
	if mirror != nil {
		mirror.cancel()
	}
}

func (e *Engine) resumeConversationMirrors(p Platform) {
	// Restore explicit/persisted bindings first, including their watermark and
	// in-flight card handles.
	for _, binding := range e.trackStore.bindings() {
		if binding == nil || !strings.EqualFold(binding.Platform, p.Name()) || binding.ThreadID == "" {
			continue
		}
		agent, sessions := e.sessionContextForKey(binding.SessionKey)
		if !supportsConversationMirror(agent, p) || !e.effectiveTrackEnabled(binding) {
			continue
		}
		active := sessions.GetOrCreateActive(binding.SessionKey)
		if active.GetAgentSessionID() != binding.ThreadID {
			continue
		}
		if err := e.validateConversationMirrorRestore(agent, binding.ThreadID); err != nil {
			slog.Warn("track: persisted mirror restore skipped", "platform", p.Name(), "thread_id", binding.ThreadID, "error", err)
			continue
		}
		e.startConversationMirror(agent, sessions, p, binding)
	}

	// Establish default-on bindings for already-persisted active sessions. This
	// never scans arbitrary backend threads: every thread comes from cc-connect's
	// own active session map.
	if !supportsConversationMirror(e.agent, p) {
		return
	}
	type defaultBindingCandidate struct {
		sessionKey string
		threadID   string
	}
	candidates := make(map[string][]defaultBindingCandidate)
	idToKey, activeIDs := e.sessions.SessionKeyMap()
	for localSessionID, sessionKey := range idToKey {
		if !activeIDs[localSessionID] || !strings.EqualFold(extractPlatformName(sessionKey), p.Name()) {
			continue
		}
		session := e.sessions.FindByID(localSessionID)
		if session == nil || session.GetAgentSessionID() == "" {
			continue
		}
		destination, err := mirrorDestinationKey(p, sessionKey)
		if err != nil || e.trackStore.binding(destination) != nil {
			continue
		}
		candidates[destination] = append(candidates[destination], defaultBindingCandidate{
			sessionKey: sessionKey,
			threadID:   session.GetAgentSessionID(),
		})
	}
	for destination, destinationCandidates := range candidates {
		if len(destinationCandidates) != 1 {
			slog.Warn("track: default binding is ambiguous; waiting for an active message or /track on",
				"platform", p.Name(), "destination", destination, "candidate_count", len(destinationCandidates))
			continue
		}
		candidate := destinationCandidates[0]
		provisional := &trackBindingState{
			Destination: destination,
			SessionKey:  candidate.sessionKey,
			Platform:    p.Name(),
			ThreadID:    candidate.threadID,
		}
		if e.effectiveTrackEnabled(provisional) {
			if err := e.validateConversationMirrorRestore(e.agent, candidate.threadID); err != nil {
				slog.Warn("track: default mirror restore skipped", "platform", p.Name(), "thread_id", candidate.threadID, "error", err)
				continue
			}
		}
		binding, err := e.bindConversationMirror(p, candidate.sessionKey, candidate.threadID)
		if err != nil {
			slog.Warn("track: restore default binding failed", "platform", p.Name(), "error", err)
			continue
		}
		e.startConversationMirror(e.agent, e.sessions, p, binding)
	}
}

// validateConversationMirrorRestore prevents a persisted or default-on mirror
// from entering a permanent reconnect loop with an agent scoped to another
// workspace. Foreground messages still start mirrors through their freshly
// resolved workspace agent, so a skipped binding can recover naturally when
// that destination is used again.
func (e *Engine) validateConversationMirrorRestore(agent Agent, threadID string) error {
	provider, ok := agent.(ConversationProvider)
	if !ok {
		return ErrNotSupported
	}
	ctx, cancel := context.WithTimeout(e.ctx, 6*time.Second)
	defer cancel()
	snapshot, err := provider.GetConversation(ctx, threadID, 1)
	if err != nil {
		return err
	}
	if snapshot == nil {
		return fmt.Errorf("track: conversation %q returned an empty snapshot", threadID)
	}
	if snapshot.SessionID != "" && snapshot.SessionID != threadID {
		return fmt.Errorf("track: conversation identity mismatch: got %q, want %q", snapshot.SessionID, threadID)
	}
	return nil
}

func (e *Engine) runConversationMirror(ctx context.Context, mirror *conversationMirror, agent Agent, sessions *SessionManager, p Platform) {
	defer func() {
		e.trackMu.Lock()
		if e.conversationMirrors[mirror.destination] == mirror {
			delete(e.conversationMirrors, mirror.destination)
		}
		e.trackMu.Unlock()
	}()

	provider := agent.(ConversationProvider)
	source := agent.(ConversationEventSource)
	backoff := time.Second
	for ctx.Err() == nil {
		if err := e.reconcileConversationMirror(ctx, mirror, provider, sessions, p); err != nil && ctx.Err() == nil {
			slog.Warn("track: mirror reconcile failed", "platform", p.Name(), "error", err)
		}
		events, err := source.WatchConversation(ctx, mirror.threadID)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrNotSupported) {
				slog.Warn("track: observer connect failed", "platform", p.Name(), "error", err)
			}
			if !waitConversationMirror(ctx, backoff) {
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		watchdog := time.NewTicker(15 * time.Second)
		connected := true
		for connected {
			select {
			case <-ctx.Done():
				watchdog.Stop()
				return
			case _, ok := <-events:
				if !ok {
					connected = false
					continue
				}
				mirror.signal()
			case <-mirror.wake:
				if err := e.reconcileConversationMirror(ctx, mirror, provider, sessions, p); err != nil && ctx.Err() == nil {
					slog.Warn("track: event reconcile failed", "platform", p.Name(), "error", err)
				}
			case <-watchdog.C:
				if err := e.reconcileConversationMirror(ctx, mirror, provider, sessions, p); err != nil && ctx.Err() == nil {
					slog.Warn("track: watchdog reconcile failed", "platform", p.Name(), "error", err)
				}
			}
		}
		watchdog.Stop()
		// A cleanly closed observer is still a disconnected observer. Throttle
		// reconnects just like connection errors so an immediately closed event
		// source cannot become a daemon/CPU hot loop.
		if !waitConversationMirror(ctx, backoff) {
			return
		}
	}
}

func waitConversationMirror(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (e *Engine) reconcileConversationMirror(ctx context.Context, mirror *conversationMirror, provider ConversationProvider, sessions *SessionManager, p Platform) error {
	mirror.mu.Lock()
	defer mirror.mu.Unlock()
	binding := e.trackStore.binding(mirror.destination)
	if binding == nil || binding.ThreadID != mirror.threadID || binding.Generation != mirror.generation || !e.effectiveTrackEnabled(binding) {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	coverageProven := true
	var snapshot *ConversationSnapshot
	var err error
	if binding.Initialized && binding.Watermark != "" {
		if windowProvider, ok := provider.(ConversationWindowProvider); ok {
			snapshot, coverageProven, err = windowProvider.GetConversationWindow(readCtx, mirror.threadID, binding.Watermark, trackReconcileMaxTurns)
		} else {
			snapshot, err = provider.GetConversation(readCtx, mirror.threadID, trackRecentTurnLimit)
		}
	} else {
		snapshot, err = provider.GetConversation(readCtx, mirror.threadID, trackRecentTurnLimit)
	}
	cancel()
	if err != nil {
		return err
	}
	if snapshot == nil || snapshot.SessionID != mirror.threadID {
		return fmt.Errorf("track: backend returned an unexpected conversation")
	}
	if !coverageProven {
		_ = e.trackStore.setGap(binding.Destination, "watermark_not_covered")
		var active []ConversationTurn
		for _, turn := range snapshot.Turns {
			if delivery := e.trackStore.delivery(binding.Destination, binding.ThreadID, turn.ID, "primary"); delivery != nil && !delivery.Terminal {
				active = append(active, turn)
			}
		}
		return e.deliverConversationCandidates(ctx, mirror, binding, snapshot, active, sessions, p)
	}
	if err := e.trackStore.clearGap(binding.Destination); err != nil {
		return err
	}

	turns := snapshot.Turns
	var candidates []ConversationTurn
	if !binding.Initialized {
		watermark := ""
		recent := make([]string, 0, len(turns))
		for _, turn := range turns {
			recent = append(recent, turn.ID)
			if conversationTurnTerminal(turn.Status) || turn.Status == ConversationTurnUnknown {
				watermark = turn.ID
			}
		}
		if err := e.trackStore.setInitialized(binding.Destination, watermark, recent); err != nil {
			return err
		}
		binding = e.trackStore.binding(binding.Destination)
		for _, turn := range turns {
			if !conversationTurnTerminal(turn.Status) && turn.Status != ConversationTurnUnknown {
				candidates = append(candidates, turn)
			}
		}
	} else {
		watermarkIndex := -1
		if binding.Watermark != "" {
			for i := range turns {
				if turns[i].ID == binding.Watermark {
					watermarkIndex = i
					break
				}
			}
			if watermarkIndex < 0 {
				_ = e.trackStore.setGap(binding.Destination, "watermark_not_covered")
				// Existing active deliveries may still be safely refreshed, but do
				// not advance across an unproven history gap.
				for _, turn := range turns {
					if delivery := e.trackStore.delivery(binding.Destination, binding.ThreadID, turn.ID, "primary"); delivery != nil && !delivery.Terminal {
						candidates = append(candidates, turn)
					}
				}
				return e.deliverConversationCandidates(ctx, mirror, binding, snapshot, candidates, sessions, p)
			}
		}
		candidates = append(candidates, turns[watermarkIndex+1:]...)
		for _, turn := range turns {
			if delivery := e.trackStore.delivery(binding.Destination, binding.ThreadID, turn.ID, "primary"); delivery != nil && !delivery.Terminal && !containsConversationTurn(candidates, turn.ID) {
				candidates = append(candidates, turn)
			}
		}
	}
	return e.deliverConversationCandidates(ctx, mirror, binding, snapshot, candidates, sessions, p)
}

func containsConversationTurn(turns []ConversationTurn, turnID string) bool {
	for _, turn := range turns {
		if turn.ID == turnID {
			return true
		}
	}
	return false
}

func (e *Engine) deliverConversationCandidates(ctx context.Context, mirror *conversationMirror, binding *trackBindingState, snapshot *ConversationSnapshot, turns []ConversationTurn, sessions *SessionManager, p Platform) error {
	for _, turn := range turns {
		if strings.TrimSpace(turn.ID) == "" {
			continue
		}
		processed, err := e.deliverConversationTurn(ctx, mirror, binding, snapshot, turn, sessions, p)
		if err != nil {
			return err
		}
		if !processed {
			// Source identity is not authoritative yet. Preserve ordering and
			// leave the watermark untouched until a later event/snapshot.
			return nil
		}
		if err := e.trackStore.markTurnObserved(binding.Destination, turn.ID); err != nil {
			return err
		}
	}
	return nil
}

func conversationTurnClientID(turn ConversationTurn) string {
	for _, message := range turn.Messages {
		if message.Role == "user" && strings.TrimSpace(message.ClientID) != "" {
			return strings.TrimSpace(message.ClientID)
		}
	}
	return ""
}

func foregroundReservationMatchesBinding(reservation *trackForegroundReservation, binding *trackBindingState) bool {
	return reservation != nil && binding != nil &&
		reservation.Destination == binding.Destination &&
		reservation.SessionKey == binding.SessionKey &&
		reservation.ThreadID == binding.ThreadID &&
		reservation.Generation == binding.Generation
}

func (e *Engine) deliverConversationTurn(ctx context.Context, mirror *conversationMirror, binding *trackBindingState, snapshot *ConversationSnapshot, turn ConversationTurn, sessions *SessionManager, p Platform) (bool, error) {
	delivery := e.trackStore.delivery(binding.Destination, binding.ThreadID, turn.ID, "primary")
	if delivery != nil && delivery.Source == "foreground" {
		_, err := e.trackStore.setDeliveryRender(delivery.Key, "", string(turn.Status), conversationTurnTerminal(turn.Status) || turn.Status == ConversationTurnUnknown)
		return true, err
	}

	clientID := conversationTurnClientID(turn)
	reservation := e.trackStore.foregroundReservation(clientID)
	if !foregroundReservationMatchesBinding(reservation, binding) {
		reservation = nil
	}
	if reservation == nil && clientID == "" {
		reservation = e.trackStore.foregroundReservationForTurn(binding.ThreadID, turn.ID)
		if !foregroundReservationMatchesBinding(reservation, binding) {
			reservation = nil
		}
	}
	if reservation == nil && delivery == nil && clientID == "" {
		active := sessions.GetOrCreateActive(binding.SessionKey)
		if active.GetAgentSessionID() == binding.ThreadID && active.Busy() {
			return false, nil
		}
	}
	if reservation != nil {
		if clientID == "" {
			clientID = reservation.ClientID
		}
		if err := e.trackStore.confirmForegroundTurn(clientID, binding.ThreadID, turn.ID); err != nil {
			return false, err
		}
		claimed, _, err := e.trackStore.claimDelivery(binding, turn.ID, "primary", "foreground", clientID)
		if err != nil {
			return false, err
		}
		if claimed != nil {
			_, err = e.trackStore.setDeliveryRender(claimed.Key, "", string(turn.Status), conversationTurnTerminal(turn.Status) || turn.Status == ConversationTurnUnknown)
		}
		return true, err
	}
	if delivery == nil && conversationPrompt(turn) == "" {
		return false, nil
	}
	if delivery == nil {
		var err error
		delivery, _, err = e.trackStore.claimDelivery(binding, turn.ID, "primary", "external", clientID)
		if err != nil {
			return false, err
		}
	}
	if delivery.Source != "external" {
		return true, nil
	}
	return true, e.updateExternalConversationCard(ctx, mirror, binding, snapshot, turn, delivery, p)
}

func (e *Engine) updateExternalConversationCard(ctx context.Context, mirror *conversationMirror, binding *trackBindingState, snapshot *ConversationSnapshot, turn ConversationTurn, delivery *trackDeliveryState, p Platform) error {
	reconstructor, ok := p.(ReplyContextReconstructor)
	if !ok {
		return fmt.Errorf("track: platform cannot reconstruct a proactive target")
	}
	replyCtx, err := reconstructor.ReconstructReplyCtx(binding.Destination)
	if err != nil {
		return err
	}
	terminal := conversationTurnTerminal(turn.Status) || turn.Status == ConversationTurnUnknown
	separateResult := terminal && e.shouldNotifyTrackTerminal(turn.Status)
	includeResponse := terminal && !separateResult
	markdown := e.renderTrackMarkdownWithResponse(snapshot, turn, includeResponse)
	payload := e.renderTrackPayloadWithResponse(p, snapshot, turn, markdown, binding.SessionKey, includeResponse)
	renderHash := trackRenderHash(payload)
	cardFailed := strings.HasPrefix(delivery.LastError, "terminal_card_")

	handle := mirror.handles[delivery.Key]
	if handle == nil && delivery.CardHandle != "" {
		codec, ok := p.(PreviewHandleCodec)
		if !ok {
			_ = e.trackStore.setDeliveryError(delivery.Key, "handle_restore_unsupported")
			return fmt.Errorf("track: platform cannot restore an existing card handle")
		}
		handle, err = codec.RestorePreviewHandle(delivery.CardHandle)
		if err != nil {
			_ = e.trackStore.setDeliveryError(delivery.Key, "handle_restore_failed")
			return err
		}
		mirror.handles[delivery.Key] = handle
		if delivery.CardMessageID == "" {
			if messageID, identifyErr := previewMessageID(p, handle); identifyErr == nil {
				if updated, persistErr := e.trackStore.setDeliveryHandle(delivery.Key, delivery.CardHandle, messageID); persistErr == nil {
					delivery = updated
				}
			}
		}
	}
	created := false
	if handle == nil {
		if err := e.waitOutgoing(p); err != nil {
			return err
		}
		if starter, ok := p.(IdempotentPreviewStarter); ok {
			handle, err = starter.SendPreviewStartIdempotent(ctx, replyCtx, payload, delivery.CardCreateKey)
		} else if starter, ok := p.(PreviewStarter); ok {
			handle, err = starter.SendPreviewStart(ctx, replyCtx, payload)
		} else {
			err = ErrNotSupported
		}
		if err != nil || handle == nil {
			if err == nil {
				err = fmt.Errorf("track: platform returned an empty card handle")
			}
			if terminal {
				delivery, persistErr := e.trackStore.setDeliveryRender(delivery.Key, renderHash, string(turn.Status), true)
				if persistErr != nil {
					return persistErr
				}
				_ = e.trackStore.setDeliveryError(delivery.Key, "terminal_card_create_failed")
				return e.sendTrackTerminalResult(ctx, p, replyCtx, turn, delivery, true, separateResult)
			}
			_ = e.trackStore.setDeliveryError(delivery.Key, "card_create_failed")
			return err
		}
		created = true
		mirror.handles[delivery.Key] = handle
		if codec, ok := p.(PreviewHandleCodec); ok {
			encoded, encodeErr := codec.EncodePreviewHandle(handle)
			if encodeErr != nil {
				return encodeErr
			}
			messageID, identifyErr := previewMessageID(p, handle)
			if identifyErr != nil {
				return identifyErr
			}
			delivery, err = e.trackStore.setDeliveryHandle(delivery.Key, encoded, messageID)
			if err != nil {
				return err
			}
		}
	}
	if !created && renderHash != delivery.RenderHash {
		updater, ok := p.(MessageUpdater)
		if !ok {
			return ErrNotSupported
		}
		if err := updateTrackCardWithRetry(ctx, updater, handle, payload); err != nil {
			if terminal {
				delivery, persistErr := e.trackStore.setDeliveryRender(delivery.Key, renderHash, string(turn.Status), true)
				if persistErr != nil {
					return persistErr
				}
				_ = e.trackStore.setDeliveryError(delivery.Key, "terminal_card_update_failed")
				return e.sendTrackTerminalResult(ctx, p, replyCtx, turn, delivery, true, separateResult)
			}
			_ = e.trackStore.setDeliveryError(delivery.Key, "card_update_failed")
			return err
		}
		if codec, ok := p.(PreviewHandleCodec); ok {
			if encoded, encodeErr := codec.EncodePreviewHandle(handle); encodeErr == nil {
				messageID, _ := previewMessageID(p, handle)
				if updated, persistErr := e.trackStore.setDeliveryHandle(delivery.Key, encoded, messageID); persistErr == nil {
					delivery = updated
				}
			}
		}
	}
	delivery, err = e.trackStore.setDeliveryRender(delivery.Key, renderHash, string(turn.Status), terminal)
	if err != nil {
		return err
	}
	if !terminal {
		return nil
	}
	return e.sendTrackTerminalResult(ctx, p, replyCtx, turn, delivery, cardFailed, separateResult)
}

func updateTrackCardWithRetry(ctx context.Context, updater MessageUpdater, handle any, payload string) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = updater.UpdateMessage(ctx, handle, payload); err == nil {
			return nil
		}
		if attempt == 2 {
			break
		}
		delay := time.Duration(attempt+1) * 200 * time.Millisecond
		if !waitConversationMirror(ctx, delay) {
			return ctx.Err()
		}
	}
	return err
}

func (e *Engine) sendTrackTerminalResult(ctx context.Context, p Platform, replyCtx any, turn ConversationTurn, delivery *trackDeliveryState, cardFailed, separateResult bool) error {
	if delivery == nil || delivery.NotificationState == "sent" {
		return nil
	}
	if !cardFailed && !separateResult {
		return e.trackStore.markNotificationSent(delivery.Key)
	}
	result, footer := e.renderTrackTerminalResult(p, turn, cardFailed)
	var err error
	if sender, ok := p.(IdempotentStatusFooterSender); ok {
		err = sender.SendIdempotentWithStatusFooter(ctx, replyCtx, result, footer, delivery.NotificationKey)
	} else if sender, ok := p.(IdempotentSender); ok {
		err = sender.SendIdempotent(ctx, replyCtx, appendReplyFooter(result, footer), delivery.NotificationKey)
	} else if sender, ok := p.(StatusFooterSender); ok {
		err = sender.SendWithStatusFooter(ctx, replyCtx, result, footer)
	} else {
		err = p.Send(ctx, replyCtx, appendReplyFooter(result, footer))
	}
	if err != nil {
		errorState := "terminal_notification_failed"
		if cardFailed {
			// Preserve the card-failure class so a later outbox retry still
			// sends the stronger fallback notification even under notify=never.
			errorState = "terminal_card_notification_failed"
		}
		_ = e.trackStore.setDeliveryError(delivery.Key, errorState)
		return err
	}
	return e.trackStore.markNotificationSent(delivery.Key)
}

func (e *Engine) renderTrackTerminalResult(p Platform, turn ConversationTurn, cardFailed bool) (string, string) {
	response := conversationFinalAnswer(turn)
	if strings.TrimSpace(response) == "" {
		response = conversationLiveResponse(turn)
	}
	if strings.TrimSpace(response) == "" {
		response = e.trackTerminalNotification(turn.Status)
	}
	response = e.renderTrackSection(response)
	footer := e.i18n.T(MsgTrackMirrorFooter)
	if elapsed := e.trackElapsed(turn); elapsed != "" {
		footer += "\n" + elapsed
	}
	if cardFailed {
		footer += "\n" + e.i18n.T(MsgTrackMirrorCardFailedNotification)
	}

	if resolver, ok := p.(RichCardMarkdownResolver); ok && response != "" {
		response = resolver.ResolveRichCardMarkdown(e.ctx, response, true)
	}
	return response, footer
}

func previewMessageID(p Platform, handle any) (string, error) {
	identifier, ok := p.(PreviewHandleIdentifier)
	if !ok {
		return "", fmt.Errorf("track: platform cannot expose preview message identity")
	}
	messageID, err := identifier.PreviewMessageID(handle)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(messageID) == "" {
		return "", fmt.Errorf("track: platform returned an empty preview message identity")
	}
	return strings.TrimSpace(messageID), nil
}

func (e *Engine) shouldNotifyTrackTerminal(status ConversationTurnStatus) bool {
	e.trackMu.Lock()
	notify := e.trackCfg.Notify
	e.trackMu.Unlock()
	switch notify {
	case "never":
		return false
	case "on_failure":
		return status == ConversationTurnFailed || status == ConversationTurnInterrupted
	default:
		return true
	}
}

// handleTrackedConversationInput protects shared threads from turn/start's
// same-turn steering behavior. A direct reply to an external card uses the
// backend's exact expected-turn precondition; an independent message is held
// back while any authoritative turn is active because this Codex version has
// no authoritative queue request.
func (e *Engine) handleTrackedConversationInput(p Platform, msg *Message, directContent string, agent Agent, sessions *SessionManager) bool {
	provider, canRead := agent.(ConversationProvider)
	steerer, canSteer := agent.(ConversationTurnSteerer)
	if !canRead || !canSteer || msg == nil {
		return false
	}
	session := sessions.GetOrCreateActive(msg.SessionKey)
	threadID := session.GetAgentSessionID()
	if threadID == "" {
		return false
	}
	destination, err := mirrorDestinationKey(p, msg.SessionKey)
	if err != nil {
		return false
	}
	binding := e.trackStore.binding(destination)
	var referenced *trackDeliveryState
	if msg.ReferencedMessageID != "" {
		referenced = e.trackStore.deliveryByCardMessageID(destination, msg.ReferencedMessageID)
		if referenced != nil {
			if !e.isAdmin(msg.UserID) {
				e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgAdminRequired), "/track"))
				return true
			}
			if binding == nil || referenced.Source != "external" || referenced.Purpose != "primary" ||
				referenced.ThreadID != threadID || referenced.Generation != binding.Generation ||
				referenced.CardMessageID != msg.ReferencedMessageID || referenced.Terminal {
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSteerStale))
				return true
			}
		}
	}
	// Preserve the existing in-process busy queue for a foreground cc-connect
	// turn. The authoritative check below is for turns that this process does
	// not currently own.
	if referenced == nil && session.Busy() {
		return false
	}

	readCtx, cancel := context.WithTimeout(e.ctx, 4*time.Second)
	snapshot, readErr := provider.GetConversation(readCtx, threadID, 4)
	cancel()
	if readErr != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackSharedTurnCheckFailed, readErr))
		return true
	}
	if snapshot == nil || snapshot.SessionID != threadID {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackSharedTurnCheckFailed, "unexpected conversation snapshot"))
		return true
	}
	var active ConversationTurn
	for index := len(snapshot.Turns) - 1; index >= 0; index-- {
		if snapshot.Turns[index].Status == ConversationTurnInProgress {
			active = snapshot.Turns[index]
			break
		}
	}

	if referenced == nil {
		if active.ID == "" {
			return false
		}
		if binding != nil {
			e.trackMu.Lock()
			mirror := e.conversationMirrors[destination]
			e.trackMu.Unlock()
			if mirror != nil {
				mirror.signal()
			}
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSharedTurnBusy))
		return true
	}
	if active.ID == "" || active.ID != referenced.TurnID {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSteerStale))
		return true
	}
	if strings.TrimSpace(directContent) == "" && len(msg.Images) == 0 && len(msg.Files) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSteerStale))
		return true
	}

	runMessageAccepted(msg)
	session.TouchUserActivity()
	steerCtx, steerCancel := context.WithTimeout(e.ctx, 5*time.Second)
	steerErr := steerer.SteerConversationTurn(
		steerCtx, threadID, referenced.TurnID, directContent, msg.MessageID, msg.Images, msg.Files,
	)
	steerCancel()
	if steerErr != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackSteerFailed, steerErr))
		return true
	}
	e.trackMu.Lock()
	mirror := e.conversationMirrors[destination]
	e.trackMu.Unlock()
	if mirror != nil {
		mirror.signal()
	}
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackSteerAccepted))
	return true
}

func (e *Engine) trackTerminalNotification(status ConversationTurnStatus) string {
	switch status {
	case ConversationTurnFailed:
		return e.i18n.T(MsgTrackMirrorFailedNotification)
	case ConversationTurnInterrupted:
		return e.i18n.T(MsgTrackMirrorInterruptedNotification)
	default:
		return e.i18n.T(MsgTrackMirrorCompletedNotification)
	}
}

func (e *Engine) cmdTrack(p Platform, msg *Message, args []string) {
	if len(args) > 0 {
		switch strings.ToLower(strings.TrimSpace(args[0])) {
		case "stop":
			e.cmdTrackStop(p, msg, args)
		case "on":
			e.cmdTrackOn(p, msg, args)
		case "off":
			e.cmdTrackOff(p, msg, args)
		case "status":
			e.cmdTrackStatus(p, msg, args)
		default:
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackUsage))
		}
		return
	}
	agent, sessions, interactiveKey, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	provider, ok := agent.(ConversationProvider)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackNotSupported))
		return
	}
	session := sessions.GetOrCreateActive(msg.SessionKey)
	sessionID := session.GetAgentSessionID()
	if sessionID == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackNoSession))
		return
	}

	e.cancelConversationTracker(interactiveKey)
	snapshot, err := provider.GetConversation(e.ctx, sessionID, 1)
	if err != nil {
		slog.Error("track: initial backend read failed", "session", sessionID, "error", err)
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackReadFailed, err))
		return
	}
	turn, ok := latestConversationTurn(snapshot)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackNoTurns))
		return
	}
	if destination, destinationErr := mirrorDestinationKey(p, msg.SessionKey); destinationErr == nil {
		if delivery := e.trackStore.delivery(destination, sessionID, turn.ID, "primary"); delivery != nil && delivery.Source == "external" && delivery.CardHandle != "" {
			binding := e.trackStore.binding(destination)
			e.startConversationMirror(agent, sessions, p, binding)
			e.trackMu.Lock()
			mirror := e.conversationMirrors[destination]
			e.trackMu.Unlock()
			if mirror != nil {
				mirror.signal()
			}
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackRefreshed))
			return
		}
	}

	markdown := e.renderTrackMarkdown(snapshot, turn)
	payload := e.renderTrackPayload(p, snapshot, turn, markdown, msg.SessionKey)
	starter, hasStarter := p.(PreviewStarter)
	updater, hasUpdater := p.(MessageUpdater)
	if !hasStarter || !hasUpdater {
		e.replyTrackFallback(p, msg.ReplyCtx, markdown)
		return
	}
	if err := e.waitOutgoing(p); err != nil {
		return
	}
	handle, err := starter.SendPreviewStart(e.ctx, msg.ReplyCtx, payload)
	if err != nil || handle == nil {
		if err != nil && !errors.Is(err, ErrNotSupported) {
			slog.Warn("track: start preview failed", "platform", p.Name(), "error", err)
		}
		e.replyTrackFallback(p, msg.ReplyCtx, markdown)
		return
	}
	if conversationTurnTerminal(turn.Status) || turn.Status == ConversationTurnUnknown {
		return
	}

	ctx, cancel := context.WithCancel(e.ctx)
	tracker := &conversationTracker{cancel: cancel, sessionID: sessionID, turnID: turn.ID}
	e.trackMu.Lock()
	e.trackers[interactiveKey] = tracker
	e.trackMu.Unlock()
	go e.runConversationTracker(ctx, tracker, interactiveKey, msg.SessionKey, provider, p, updater, handle, payload)
}

func (e *Engine) cmdTrackOn(p Platform, msg *Message, args []string) {
	if len(args) != 1 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackUsage))
		return
	}
	agent, sessions, _, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	if !supportsConversationMirror(agent, p) {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackNotSupported))
		return
	}
	session := sessions.GetOrCreateActive(msg.SessionKey)
	threadID := session.GetAgentSessionID()
	if threadID == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackNoSession))
		return
	}
	destination, err := mirrorDestinationKey(p, msg.SessionKey)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackPersistFailed, err))
		return
	}
	wasEnabled := e.effectiveTrackEnabled(e.trackStore.binding(destination))
	binding, err := e.bindConversationMirror(p, msg.SessionKey, threadID)
	if err == nil {
		binding, err = e.trackStore.setOverride(destination, msg.SessionKey, p.Name(), trackOverrideOn)
	}
	if err == nil && !wasEnabled {
		err = e.trackStore.resetBaseline(destination)
		binding = e.trackStore.binding(destination)
	}
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackPersistFailed, err))
		return
	}
	if !e.effectiveTrackEnabled(binding) {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackProjectDisabled))
		return
	}
	e.startConversationMirror(agent, sessions, p, binding)
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackEnabled))
}

func (e *Engine) cmdTrackOff(p Platform, msg *Message, args []string) {
	if len(args) != 1 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackUsage))
		return
	}
	destination, err := mirrorDestinationKey(p, msg.SessionKey)
	if err == nil {
		_, err = e.trackStore.setOverride(destination, msg.SessionKey, p.Name(), trackOverrideOff)
	}
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackPersistFailed, err))
		return
	}
	// Persist first, then cancel. A crash between these operations still comes
	// back disabled on restart.
	e.stopConversationMirror(destination)
	e.cancelConversationTracker(e.interactiveKeyForSessionKey(msg.SessionKey))
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackDisabled))
}

func (e *Engine) cmdTrackStatus(p Platform, msg *Message, args []string) {
	if len(args) != 1 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackUsage))
		return
	}
	agent, sessions, _, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	destination, err := mirrorDestinationKey(p, msg.SessionKey)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackPersistFailed, err))
		return
	}
	binding := e.trackStore.binding(destination)
	threadID := sessions.GetOrCreateActive(msg.SessionKey).GetAgentSessionID()
	if binding == nil {
		binding = &trackBindingState{Destination: destination, SessionKey: msg.SessionKey, Platform: p.Name(), ThreadID: threadID}
	}
	override := string(binding.Override)
	if override == "" {
		override = "inherit"
	}
	effective := "off"
	if e.effectiveTrackEnabled(binding) && supportsConversationMirror(agent, p) {
		effective = "on"
	}
	gap := binding.Gap
	if gap == "" {
		gap = "none"
	}
	lastTurn := binding.LastTurnID
	if lastTurn == "" {
		lastTurn = "none"
	}
	if binding.ThreadID != "" {
		threadID = binding.ThreadID
	}
	if threadID == "" {
		threadID = "none"
	}
	realtime := capabilityLabel(false)
	if _, ok := agent.(ConversationEventSource); ok {
		realtime = capabilityLabel(true)
	}
	pagedReconcile := capabilityLabel(false)
	if _, ok := agent.(ConversationWindowProvider); ok {
		pagedReconcile = capabilityLabel(true)
	}
	clientMarker := capabilityLabel(false)
	if supporter, ok := agent.(ConversationClientMarkerSupport); ok && supporter.SupportsConversationClientMarker() {
		clientMarker = capabilityLabel(true)
	}
	cardRecovery := capabilityLabel(false)
	if _, ok := p.(PreviewHandleCodec); ok {
		if _, idempotent := p.(IdempotentPreviewStarter); idempotent {
			if _, identifiable := p.(PreviewHandleIdentifier); identifiable {
				cardRecovery = capabilityLabel(true)
			}
		}
	}
	exactSteer := capabilityLabel(false)
	if _, ok := agent.(ConversationTurnSteerer); ok {
		exactSteer = capabilityLabel(true)
	}
	daemonQueue := capabilityLabel(false)
	if _, ok := agent.(ConversationInputQueue); ok {
		daemonQueue = capabilityLabel(true)
	}
	exactInterrupt := capabilityLabel(false)
	if _, ok := agent.(ConversationTurnController); ok {
		exactInterrupt = capabilityLabel(true)
	}
	e.trackMu.Lock()
	sharedWrite := e.trackCfg.SharedWrite
	defaultEnabled := e.trackCfg.DefaultEnabled
	e.trackMu.Unlock()
	if sharedWrite == "daemon_queue" && daemonQueue == capabilityLabel(false) {
		// The status remains explicit until the backend advertises the queue
		// capability; configuration alone must not imply safety.
		sharedWrite = "observer_only (queue unavailable)"
	}
	defaultLabel := "off"
	if defaultEnabled {
		defaultLabel = "on"
	}
	report := e.i18n.Tf(MsgTrackStatusReport,
		effective, defaultLabel, override, threadID, lastTurn, gap, realtime, pagedReconcile, clientMarker,
		cardRecovery, exactSteer, daemonQueue, sharedWrite, exactInterrupt)
	e.reply(p, msg.ReplyCtx, report)
}

func capabilityLabel(available bool) string {
	if available {
		return "✅"
	}
	return "❌"
}

func (e *Engine) cmdTrackStop(p Platform, msg *Message, args []string) {
	if len(args) != 2 && len(args) != 3 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackUsage))
		return
	}
	agent, sessions, interactiveKey, err := e.commandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	provider, canRead := agent.(ConversationProvider)
	controller, canInterrupt := agent.(ConversationTurnController)
	if !canRead || !canInterrupt {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackInterruptNotSupported))
		return
	}
	var sessionID, turnID string
	if len(args) == 3 {
		sessionID = strings.TrimSpace(args[1])
		turnID = strings.TrimSpace(args[2])
	}
	e.trackMu.Lock()
	tracker := e.trackers[interactiveKey]
	validTracker := len(args) == 3 && tracker != nil && tracker.sessionID == sessionID && tracker.turnID == turnID
	e.trackMu.Unlock()
	validDelivery := false
	if destination, destinationErr := mirrorDestinationKey(p, msg.SessionKey); destinationErr == nil {
		binding := e.trackStore.binding(destination)
		var delivery *trackDeliveryState
		if len(args) == 2 {
			delivery = e.trackStore.deliveryByKey(args[1])
		} else {
			delivery = e.trackStore.delivery(destination, sessionID, turnID, "primary")
		}
		validDelivery = binding != nil && delivery != nil && delivery.Source == "external" && delivery.Purpose == "primary" &&
			delivery.Destination == destination && binding.ThreadID == delivery.ThreadID && binding.Generation == delivery.Generation &&
			!delivery.Terminal && (msg.ReferencedMessageID == "" || delivery.CardMessageID == msg.ReferencedMessageID)
		if validDelivery {
			sessionID = delivery.ThreadID
			turnID = delivery.TurnID
		}
	}
	if (!validTracker && !validDelivery) || sessions.GetOrCreateActive(msg.SessionKey).GetAgentSessionID() != sessionID {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackInterruptStale))
		return
	}

	snapshot, err := provider.GetConversation(e.ctx, sessionID, 1)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackReadFailed, err))
		return
	}
	turn, ok := conversationTurnByID(snapshot, turnID)
	if !ok || turn.Status != ConversationTurnInProgress {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackInterruptStale))
		return
	}
	interruptCtx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	defer cancel()
	if err := controller.InterruptConversationTurn(interruptCtx, sessionID, turnID); err != nil {
		slog.Error("track: interrupt failed", "session", sessionID, "turn", turnID, "error", err)
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgTrackInterruptFailed, err))
		return
	}
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgTrackInterruptRequested))
}

func (e *Engine) cancelConversationTracker(interactiveKey string) {
	e.trackMu.Lock()
	tracker := e.trackers[interactiveKey]
	if tracker != nil {
		delete(e.trackers, interactiveKey)
	}
	e.trackMu.Unlock()
	if tracker != nil {
		tracker.cancel()
	}
}

func (e *Engine) runConversationTracker(ctx context.Context, tracker *conversationTracker, interactiveKey, sessionKey string, provider ConversationProvider, platform Platform, updater MessageUpdater, handle any, lastPayload string) {
	defer func() {
		e.trackMu.Lock()
		if e.trackers[interactiveKey] == tracker {
			delete(e.trackers, interactiveKey)
		}
		e.trackMu.Unlock()
	}()

	interval := time.Duration(e.streamPreview.IntervalMs) * time.Millisecond
	if interval < 1500*time.Millisecond {
		interval = 1500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		_, sessions := e.sessionContextForKey(sessionKey)
		if sessions.GetOrCreateActive(sessionKey).GetAgentSessionID() != tracker.sessionID {
			return
		}
		snapshot, err := provider.GetConversation(ctx, tracker.sessionID, 1)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("track: backend refresh failed", "session", tracker.sessionID, "turn", tracker.turnID, "error", err)
			}
			continue
		}
		turn, ok := conversationTurnByID(snapshot, tracker.turnID)
		if !ok {
			slog.Warn("track: pinned turn disappeared", "session", tracker.sessionID, "turn", tracker.turnID)
			return
		}
		markdown := e.renderTrackMarkdown(snapshot, turn)
		payload := e.renderTrackPayload(platform, snapshot, turn, markdown, sessionKey)
		if payload != lastPayload {
			if ctx.Err() != nil {
				return
			}
			if err := updater.UpdateMessage(ctx, handle, payload); err != nil {
				slog.Warn("track: preview update failed", "platform", platform.Name(), "error", err)
			} else {
				lastPayload = payload
			}
		}
		if conversationTurnTerminal(turn.Status) || turn.Status == ConversationTurnUnknown {
			return
		}
	}
}

func latestConversationTurn(snapshot *ConversationSnapshot) (ConversationTurn, bool) {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return ConversationTurn{}, false
	}
	return snapshot.Turns[len(snapshot.Turns)-1], true
}

func conversationTurnByID(snapshot *ConversationSnapshot, turnID string) (ConversationTurn, bool) {
	if snapshot == nil {
		return ConversationTurn{}, false
	}
	for i := len(snapshot.Turns) - 1; i >= 0; i-- {
		if snapshot.Turns[i].ID == turnID {
			return snapshot.Turns[i], true
		}
	}
	return ConversationTurn{}, false
}

func (e *Engine) renderTrackMarkdown(snapshot *ConversationSnapshot, turn ConversationTurn) string {
	return e.renderTrackMarkdownWithResponse(snapshot, turn, true)
}

func (e *Engine) renderTrackMarkdownWithResponse(snapshot *ConversationSnapshot, turn ConversationTurn, includeResponse bool) string {
	prompt := e.renderTrackSection(conversationPrompt(turn))
	if prompt == "" {
		prompt = e.i18n.T(MsgTrackContentPending)
	}

	var sections []string
	sections = append(sections, e.i18n.Tf(MsgTrackPromptSection, prompt))
	if includeResponse {
		response := e.renderTrackSection(conversationLiveResponse(turn))
		if response == "" {
			response = e.i18n.T(MsgTrackContentPending)
		}
		sections = append(sections, e.i18n.Tf(MsgTrackResponseSection, response))
	}
	if len(turn.Activities) > 0 {
		activity := turn.Activities[len(turn.Activities)-1]
		sections = append(sections, e.i18n.Tf(MsgTrackActivitySection,
			e.trackActivityLabel(activity.Kind), e.trackStatusLabel(normalizedTrackStatus(activity.Status))))
	}
	if waiting := e.trackWaitingLabel(snapshot.ActiveFlags); waiting != "" {
		sections = append(sections, waiting)
	}
	sections = append(sections, e.i18n.Tf(MsgTrackStatusSection, e.trackStatusLabel(string(turn.Status))))
	return strings.Join(sections, "\n\n")
}

func (e *Engine) conversationTurnPresentation(turn ConversationTurn) richTurnPresentation {
	events := turn.PresentationEvents
	if len(events) == 0 {
		events = e.fallbackConversationPresentationEvents(turn)
	}
	return richTurnPresentationFromEvents(events, e.display)
}

func (e *Engine) fallbackConversationPresentationEvents(turn ConversationTurn) []Event {
	var events []Event
	var finalResponse string
	for _, message := range turn.Messages {
		if message.Role != "assistant" || strings.TrimSpace(message.Content) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(message.Phase)) {
		case "final_answer", "finalanswer":
			finalResponse = message.Content
		case "commentary":
			events = append(events, Event{Type: EventThinking, ItemID: message.ID, Content: message.Content})
		case "":
			finalResponse = message.Content
		}
	}
	for _, activity := range turn.Activities {
		name := strings.TrimSpace(activity.Name)
		if name == "" {
			name = e.trackActivityLabel(activity.Kind)
		}
		events = append(events, Event{
			Type: EventToolUse, ItemID: activity.ID,
			ToolName: name, ToolInput: activity.Summary,
		})
		status := normalizedTrackStatus(activity.Status)
		if status != "in_progress" || activity.Result != "" || activity.ExitCode != nil || activity.Success != nil {
			events = append(events, Event{
				Type: EventToolResult, ItemID: activity.ID,
				ToolName: name, ToolInput: activity.Summary, ToolResult: activity.Result,
				ToolStatus: status, ToolExitCode: activity.ExitCode, ToolSuccess: activity.Success,
			})
		}
	}
	if strings.TrimSpace(finalResponse) != "" {
		events = append(events, Event{Type: EventText, Content: finalResponse})
	}
	return events
}

func (e *Engine) renderRichTrackMarkdown(turn ConversationTurn, presentation richTurnPresentation, includeResponse bool) string {
	prompt := e.renderTrackSection(conversationPrompt(turn))
	if prompt == "" {
		prompt = e.i18n.T(MsgTrackContentPending)
	}
	sections := []string{e.i18n.Tf(MsgTrackPromptSection, prompt)}
	if !includeResponse {
		return strings.Join(sections, "\n\n")
	}
	response := e.renderTrackSection(presentation.Markdown)
	if response == "" {
		response = e.i18n.T(MsgTrackContentPending)
	}
	sections = append(sections, e.i18n.Tf(MsgTrackResponseSection, response))
	return strings.Join(sections, "\n\n")
}

func (e *Engine) renderTrackSection(content string) string {
	content = strings.TrimSpace(content)
	if len(content) <= trackSectionMaxBytes {
		return content
	}
	marker := e.i18n.T(MsgTrackContentTruncated)
	separator := "\n\n"
	contentBudget := trackSectionMaxBytes - len(separator) - len(marker)
	if contentBudget <= 0 {
		return truncateUTF8Bytes(marker, trackSectionMaxBytes)
	}
	return strings.TrimSpace(truncateUTF8Bytes(content, contentBudget)) + separator + marker
}

func truncateUTF8Bytes(content string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(content) <= maxBytes {
		return content
	}
	cut := maxBytes
	for cut > 0 && content[cut]&0xc0 == 0x80 {
		cut--
	}
	return content[:cut]
}

func (e *Engine) renderTrackPayload(p Platform, snapshot *ConversationSnapshot, turn ConversationTurn, markdown, sessionKey string) string {
	return e.renderTrackPayloadWithResponse(p, snapshot, turn, markdown, sessionKey, true)
}

func (e *Engine) renderTrackPayloadWithResponse(p Platform, snapshot *ConversationSnapshot, turn ConversationTurn, markdown, sessionKey string, includeResponse bool) string {
	_, ok := p.(RichCardSupporter)
	_, hasOptions := p.(RichCardOptionsSupporter)
	if !ok && !hasOptions {
		return markdown
	}
	presentation := e.conversationTurnPresentation(turn)
	richMarkdown := e.renderRichTrackMarkdown(turn, presentation, includeResponse)
	footer := e.i18n.T(MsgTrackMirrorFooter)
	if elapsed := e.trackElapsed(turn); elapsed != "" {
		footer += "\n" + elapsed
	}
	if waiting := e.trackWaitingLabel(snapshot.ActiveFlags); waiting != "" {
		footer += "\n" + waiting
	}
	if !includeResponse && (conversationTurnTerminal(turn.Status) || turn.Status == ConversationTurnUnknown) {
		footer += "\n" + e.i18n.T(MsgTrackMirrorResultFollows)
	}
	status := trackCardStatus(turn.Status)
	streaming := !conversationTurnTerminal(turn.Status) && turn.Status != ConversationTurnUnknown
	var buttons []CardButton
	if streaming {
		action := "cmd:/track stop " + snapshot.SessionID + " " + turn.ID
		if destination, err := mirrorDestinationKey(p, sessionKey); err == nil {
			if delivery := e.trackStore.delivery(destination, snapshot.SessionID, turn.ID, "primary"); delivery != nil && delivery.Source == "external" {
				action = "cmd:/track stop " + delivery.Key
			}
		}
		buttons = []CardButton{{
			Text:  e.i18n.T(MsgTrackInterruptButton),
			Type:  "danger",
			Value: action,
			Extra: map[string]string{"session_key": sessionKey},
		}}
	}
	options := RichCardRenderOptions{
		Status: status, Title: e.trackMirrorTitle(turn.Status), Variant: CardVariantMirror,
		Steps:             presentation.Steps,
		ProgressItems:     append([]ProgressCardEntry{}, presentation.ProgressItems...),
		ProgressTruncated: presentation.ProgressTruncated,
		Language:          e.i18n.CurrentLang(),
		Markdown:          richMarkdown,
		Streaming:         streaming,
		StatusFooter:      footer,
		Buttons:           buttons,
	}
	if resolver, ok := p.(RichCardMarkdownResolver); ok && options.Markdown != "" {
		options.Markdown = resolver.ResolveRichCardMarkdown(e.ctx, options.Markdown, !streaming)
	}
	if card, rendered := buildRichCardFrame(p, options); rendered {
		return card
	}
	return markdown
}

func trackCardStatus(status ConversationTurnStatus) CardStatus {
	switch status {
	case ConversationTurnCompleted:
		return CardStatusDone
	case ConversationTurnFailed:
		return CardStatusError
	case ConversationTurnInterrupted:
		return CardStatusInterrupted
	case ConversationTurnUnknown:
		return CardStatusPaused
	default:
		return CardStatusWorking
	}
}

func (e *Engine) trackMirrorTitle(status ConversationTurnStatus) string {
	return e.i18n.T(MsgTrackMirrorTitle) + " · " + e.trackStatusLabel(string(status))
}

func (e *Engine) replyTrackFallback(p Platform, replyCtx any, markdown string) {
	if supportsCards(p) {
		e.replyWithCard(p, replyCtx, NewCard().Title(e.i18n.T(MsgTrackTitle), "turquoise").Markdown(markdown).Build())
		return
	}
	e.reply(p, replyCtx, markdown)
}

func (e *Engine) trackElapsed(turn ConversationTurn) string {
	start := turn.StartedAt
	if start.IsZero() {
		return ""
	}
	end := time.Now()
	streaming := !conversationTurnTerminal(turn.Status)
	if !turn.CompletedAt.IsZero() {
		end = turn.CompletedAt
		streaming = false
	}
	return formatElapsed(end.Sub(start), streaming, e.i18n.currentLang())
}

func normalizedTrackStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "inprogress", "in_progress", "running", "started":
		return "in_progress"
	case "completed", "complete", "success", "succeeded":
		return "completed"
	case "failed", "error", "errored":
		return "failed"
	case "interrupted", "cancelled", "canceled":
		return "interrupted"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func (e *Engine) trackStatusLabel(status string) string {
	switch normalizedTrackStatus(status) {
	case "in_progress":
		return e.i18n.T(MsgTrackStatusInProgress)
	case "completed":
		return e.i18n.T(MsgTrackStatusCompleted)
	case "failed":
		return e.i18n.T(MsgTrackStatusFailed)
	case "interrupted":
		return e.i18n.T(MsgTrackStatusInterrupted)
	default:
		return e.i18n.T(MsgTrackStatusUnknown)
	}
}

func (e *Engine) trackActivityLabel(kind string) string {
	switch kind {
	case "shell":
		return e.i18n.T(MsgTrackActivityShell)
	case "mcp":
		return e.i18n.T(MsgTrackActivityMCP)
	case "web_search":
		return e.i18n.T(MsgTrackActivityWebSearch)
	case "file_change":
		return e.i18n.T(MsgTrackActivityFileChange)
	case "agent":
		return e.i18n.T(MsgTrackActivityAgent)
	case "plan":
		return e.i18n.T(MsgTrackActivityPlan)
	default:
		return e.i18n.T(MsgTrackActivityTool)
	}
}

func (e *Engine) trackWaitingLabel(flags []string) string {
	var labels []string
	for _, flag := range flags {
		switch strings.ToLower(strings.TrimSpace(flag)) {
		case "waitingonapproval", "waiting_on_approval":
			labels = append(labels, e.i18n.T(MsgTrackWaitingApproval))
		case "waitingonuserinput", "waiting_on_user_input":
			labels = append(labels, e.i18n.T(MsgTrackWaitingUserInput))
		}
	}
	if len(labels) == 0 {
		return ""
	}
	return e.i18n.Tf(MsgTrackWaitingSection, strings.Join(labels, ", "))
}
