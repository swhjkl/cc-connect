package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	progressStyleLegacy  = "legacy"
	progressStyleCompact = "compact"
	progressStyleCard    = "card"

	// ProgressCardPayloadPrefix marks a structured payload for card-style progress.
	ProgressCardPayloadPrefix = "__cc_connect_progress_card_v1__:"

	// Keep a margin below platform hard limit for markdown wrappers/code fences.
	compactProgressMaxChars = maxPlatformMessageLen - 200

	// Bound each platform progress-card API call so a hung upstream request
	// does not block the whole turn forever.
	compactProgressAPITimeout = 15 * time.Second

	progressRetryInitialDelay = 2 * time.Second
	progressRetryMaxDelay     = 30 * time.Second
)

type ProgressCardState string

const (
	ProgressCardStateRunning     ProgressCardState = "running"
	ProgressCardStateCompleted   ProgressCardState = "completed"
	ProgressCardStateFailed      ProgressCardState = "failed"
	ProgressCardStateInterrupted ProgressCardState = "interrupted"
)

// ProgressCardHealth describes whether a running card's authoritative turn
// state can currently be verified. It is intentionally independent from
// ProgressCardState: a turn may still be running while its observer reconnects.
type ProgressCardHealth string

const (
	ProgressCardHealthChecking     ProgressCardHealth = "checking"
	ProgressCardHealthVerified     ProgressCardHealth = "verified"
	ProgressCardHealthReconnecting ProgressCardHealth = "reconnecting"
	ProgressCardHealthUnknown      ProgressCardHealth = "unknown"
)

type ProgressCardEntryKind string

const (
	ProgressEntryInfo       ProgressCardEntryKind = "info"
	ProgressEntryThinking   ProgressCardEntryKind = "thinking"
	ProgressEntryToolUse    ProgressCardEntryKind = "tool_use"
	ProgressEntryToolResult ProgressCardEntryKind = "tool_result"
	ProgressEntryError      ProgressCardEntryKind = "error"
)

type ProgressCardEntry struct {
	Kind     ProgressCardEntryKind `json:"kind"`
	Text     string                `json:"text"`
	Tool     string                `json:"tool,omitempty"`
	Status   string                `json:"status,omitempty"`
	ExitCode *int                  `json:"exit_code,omitempty"`
	Success  *bool                 `json:"success,omitempty"`
}

// ProgressCardCounts keeps cumulative lane totals even when the visible
// progress entries are bounded to the latest updates.
type ProgressCardCounts struct {
	Reasoning int `json:"reasoning,omitempty"`
	Tools     int `json:"tools,omitempty"`
	Updates   int `json:"updates,omitempty"`
}

func (c *ProgressCardCounts) add(kind ProgressCardEntryKind) {
	switch kind {
	case ProgressEntryThinking:
		c.Reasoning++
	case ProgressEntryToolUse, ProgressEntryToolResult:
		c.Tools++
	default:
		c.Updates++
	}
}

func progressCardCountsFromItems(items []ProgressCardEntry) ProgressCardCounts {
	var counts ProgressCardCounts
	for _, item := range items {
		kind := item.Kind
		if kind == "" {
			kind = ProgressEntryInfo
		}
		counts.add(kind)
	}
	return counts
}

func (c ProgressCardCounts) withVisibleMinimum(items []ProgressCardEntry) ProgressCardCounts {
	visible := progressCardCountsFromItems(items)
	if c.Reasoning < visible.Reasoning {
		c.Reasoning = visible.Reasoning
	}
	if c.Tools < visible.Tools {
		c.Tools = visible.Tools
	}
	if c.Updates < visible.Updates {
		c.Updates = visible.Updates
	}
	return c
}

// ProgressCardPayload carries structured progress entries for platforms that
// render custom progress cards.
type ProgressCardPayload struct {
	Version        int                 `json:"version,omitempty"`
	Agent          string              `json:"agent,omitempty"`
	Lang           string              `json:"lang,omitempty"`
	State          ProgressCardState   `json:"state,omitempty"`
	Entries        []string            `json:"entries,omitempty"` // legacy fallback
	Items          []ProgressCardEntry `json:"items,omitempty"`   // ordered typed events
	Counts         ProgressCardCounts  `json:"counts,omitempty"`
	ElapsedSeconds int64               `json:"elapsed_seconds,omitempty"`
	Health         ProgressCardHealth  `json:"health,omitempty"`
	LastVerifiedAt int64               `json:"last_verified_at,omitempty"`
	Truncated      bool                `json:"truncated"`
	Hint           string              `json:"hint,omitempty"`
	Buttons        []CardButton        `json:"buttons,omitempty"`
}

// BuildProgressCardPayload encodes progress entries into a transport string.
// This legacy builder keeps compatibility with old callers that only send text.
func BuildProgressCardPayload(entries []string, truncated bool) string {
	cleaned := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			cleaned = append(cleaned, entry)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	payload := ProgressCardPayload{
		Entries:   cleaned,
		Truncated: truncated,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return ProgressCardPayloadPrefix + string(b)
}

// BuildProgressCardPayloadV2 encodes ordered typed progress events.
func BuildProgressCardPayloadV2(items []ProgressCardEntry, truncated bool, agent string, lang Language, state ProgressCardState) string {
	return buildProgressCardPayloadV2(items, truncated, agent, lang, state, "", nil)
}

// BuildProgressCardPayloadWithControls encodes typed progress events together
// with optional controls for an exactly identified running turn.
func BuildProgressCardPayloadWithControls(items []ProgressCardEntry, truncated bool, agent string, lang Language, state ProgressCardState, hint string, buttons []CardButton) string {
	return buildProgressCardPayloadV2(items, truncated, agent, lang, state, hint, buttons)
}

func buildProgressCardPayloadV2(items []ProgressCardEntry, truncated bool, agent string, lang Language, state ProgressCardState, hint string, buttons []CardButton) string {
	return buildProgressCardPayloadV2WithMeta(
		items, truncated, agent, lang, state, hint, buttons,
		progressCardCountsFromItems(items), 0, "", 0,
	)
}

func buildProgressCardPayloadV2WithMeta(items []ProgressCardEntry, truncated bool, agent string, lang Language, state ProgressCardState, hint string, buttons []CardButton, counts ProgressCardCounts, elapsedSeconds int64, health ProgressCardHealth, lastVerifiedAt int64) string {
	cleaned := make([]ProgressCardEntry, 0, len(items))
	for _, item := range items {
		text := strings.TrimSpace(item.Text)
		if text == "" {
			continue
		}
		kind := item.Kind
		if kind == "" {
			kind = ProgressEntryInfo
		}
		cleaned = append(cleaned, ProgressCardEntry{
			Kind:     kind,
			Text:     text,
			Tool:     strings.TrimSpace(item.Tool),
			Status:   strings.TrimSpace(item.Status),
			ExitCode: item.ExitCode,
			Success:  item.Success,
		})
	}
	if len(cleaned) == 0 {
		return ""
	}
	if state == "" {
		state = ProgressCardStateRunning
	}
	payload := ProgressCardPayload{
		Version:        2,
		Agent:          strings.TrimSpace(agent),
		Lang:           string(lang),
		State:          state,
		Items:          cleaned,
		Counts:         counts.withVisibleMinimum(cleaned),
		ElapsedSeconds: max(elapsedSeconds, 0),
		Health:         health,
		LastVerifiedAt: max(lastVerifiedAt, 0),
		Truncated:      truncated,
		Hint:           strings.TrimSpace(hint),
		Buttons:        cloneProgressCardButtons(buttons),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return ProgressCardPayloadPrefix + string(b)
}

// ParseProgressCardPayload decodes a structured progress payload.
func ParseProgressCardPayload(content string) (*ProgressCardPayload, bool) {
	if !strings.HasPrefix(content, ProgressCardPayloadPrefix) {
		return nil, false
	}
	raw := strings.TrimPrefix(content, ProgressCardPayloadPrefix)
	var payload ProgressCardPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, false
	}
	legacy := make([]string, 0, len(payload.Entries))
	for _, entry := range payload.Entries {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			legacy = append(legacy, entry)
		}
	}
	items := make([]ProgressCardEntry, 0, len(payload.Items))
	for _, item := range payload.Items {
		item.Text = strings.TrimSpace(item.Text)
		item.Tool = strings.TrimSpace(item.Tool)
		item.Status = strings.TrimSpace(item.Status)
		if item.Text == "" {
			continue
		}
		if item.Kind == "" {
			item.Kind = ProgressEntryInfo
		}
		items = append(items, item)
	}
	if len(items) == 0 && len(legacy) > 0 {
		for _, entry := range legacy {
			items = append(items, ProgressCardEntry{
				Kind: inferLegacyEntryKind(entry),
				Text: entry,
			})
		}
	}
	if len(items) == 0 && len(legacy) == 0 {
		return nil, false
	}
	if payload.State == "" {
		payload.State = ProgressCardStateRunning
	}
	payload.Items = items
	payload.Counts = payload.Counts.withVisibleMinimum(items)
	if payload.ElapsedSeconds < 0 {
		payload.ElapsedSeconds = 0
	}
	switch payload.Health {
	case "", ProgressCardHealthChecking, ProgressCardHealthVerified, ProgressCardHealthReconnecting, ProgressCardHealthUnknown:
	default:
		payload.Health = ProgressCardHealthUnknown
	}
	if payload.LastVerifiedAt < 0 {
		payload.LastVerifiedAt = 0
	}
	payload.Entries = legacy
	payload.Hint = strings.TrimSpace(payload.Hint)
	payload.Buttons = cloneProgressCardButtons(payload.Buttons)
	if len(payload.Entries) == 0 && len(payload.Items) > 0 {
		payload.Entries = make([]string, 0, len(payload.Items))
		for _, item := range payload.Items {
			payload.Entries = append(payload.Entries, item.Text)
		}
	}
	return &payload, true
}

func cloneProgressCardButtons(buttons []CardButton) []CardButton {
	if len(buttons) == 0 {
		return nil
	}
	cloned := make([]CardButton, 0, len(buttons))
	for _, button := range buttons {
		button.Text = strings.TrimSpace(button.Text)
		button.Type = strings.TrimSpace(button.Type)
		button.Value = strings.TrimSpace(button.Value)
		if button.Text == "" || button.Value == "" {
			continue
		}
		if len(button.Extra) > 0 {
			extra := make(map[string]string, len(button.Extra))
			for key, value := range button.Extra {
				extra[key] = value
			}
			button.Extra = extra
		}
		cloned = append(cloned, button)
	}
	return cloned
}

func inferLegacyEntryKind(entry string) ProgressCardEntryKind {
	switch {
	case strings.HasPrefix(entry, "💭"):
		return ProgressEntryThinking
	case strings.HasPrefix(entry, "🔧"), strings.Contains(entry, "**Tool #"):
		return ProgressEntryToolUse
	case strings.HasPrefix(entry, "🧾"):
		return ProgressEntryToolResult
	case strings.HasPrefix(entry, "❌"):
		return ProgressEntryError
	default:
		return ProgressEntryInfo
	}
}

// compactProgressWriter coalesces intermediate progress (thinking/tool-use)
// into one editable message for platforms that support message updates.
type compactProgressWriter struct {
	mu sync.Mutex

	ctx       context.Context
	platform  Platform
	replyCtx  any
	transform func(string) string

	starter PreviewStarter
	updater MessageUpdater
	handle  any

	enabled    bool
	degraded   bool
	failed     bool
	stopped    bool
	style      string
	usePayload bool

	content        string
	entries        []string
	items          []ProgressCardEntry
	state          ProgressCardState
	agentName      string
	lang           Language
	truncated      bool
	hint           string
	buttons        []CardButton
	counts         ProgressCardCounts
	lastSent       string
	maxEntries     int
	handleObserver func(any)
	startedAt      time.Time
	closed         bool

	// Throttle message edits to avoid platform rate limits (e.g. Discord ~5 edits/5s).
	minUpdateInterval time.Duration
	lastUpdateAt      time.Time
	flushTimer        *time.Timer
	heartbeatInterval time.Duration
	heartbeatTimer    *time.Timer
	health            ProgressCardHealth
	lastVerifiedAt    time.Time
	healthMonitoring  bool
	retryTimer        *time.Timer
	nextRetryDelay    time.Duration
}

func normalizeProgressStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case "", progressStyleLegacy:
		return progressStyleLegacy
	case progressStyleCompact:
		return progressStyleCompact
	case progressStyleCard:
		return progressStyleCard
	default:
		return progressStyleLegacy
	}
}

func progressStyleForPlatform(p Platform) string {
	ps := progressStyleLegacy
	if sp, ok := p.(ProgressStyleProvider); ok {
		ps = normalizeProgressStyle(sp.ProgressStyle())
	}
	return ps
}

type progressStyleHintProvider interface {
	progressStyleHint() string
}

type progressCardPayloadHintProvider interface {
	supportsProgressCardPayloadHint() bool
}

func progressStyleForTarget(p Platform, replyCtx any) string {
	if hint, ok := replyCtx.(progressStyleHintProvider); ok {
		return normalizeProgressStyle(hint.progressStyleHint())
	}
	return progressStyleForPlatform(p)
}

func progressCardPayloadForTarget(p Platform, replyCtx any) bool {
	if hint, ok := replyCtx.(progressCardPayloadHintProvider); ok {
		return hint.supportsProgressCardPayloadHint()
	}
	if cap, ok := p.(ProgressCardPayloadSupport); ok {
		return cap.SupportsProgressCardPayload()
	}
	return false
}

// SuppressStandaloneToolResultEvent is true when a platform opts into progress
// styling (ProgressStyleProvider) but uses legacy mode. In that case tool_use
// lines are still shown, but a separate chat message for EventToolResult is
// skipped to avoid duplicate noise (e.g. Codex structured tool results on Feishu).
// Platforms without ProgressStyleProvider keep showing standalone tool results.
func SuppressStandaloneToolResultEvent(p Platform) bool {
	_, ok := p.(ProgressStyleProvider)
	if !ok {
		return false
	}
	return progressStyleForPlatform(p) == progressStyleLegacy
}

func newCompactProgressWriter(ctx context.Context, p Platform, replyCtx any, agentName string, lang Language, transform func(string) string) *compactProgressWriter {
	w := &compactProgressWriter{
		ctx:            ctx,
		platform:       p,
		replyCtx:       replyCtx,
		transform:      transform,
		style:          progressStyleForTarget(p, replyCtx),
		state:          ProgressCardStateRunning,
		agentName:      normalizeProgressAgentLabel(agentName),
		lang:           lang,
		maxEntries:     10,
		startedAt:      time.Now(),
		health:         ProgressCardHealthChecking,
		nextRetryDelay: progressRetryInitialDelay,
	}
	if throttler, ok := p.(ProgressUpdateThrottler); ok {
		w.minUpdateInterval = throttler.ProgressUpdateInterval()
	}
	if heartbeater, ok := p.(ProgressHeartbeatProvider); ok {
		w.heartbeatInterval = heartbeater.ProgressHeartbeatInterval()
	}
	if w.style != progressStyleCompact && w.style != progressStyleCard {
		slog.Debug("progress writer disabled: unsupported style", "platform", p.Name(), "style", w.style)
		return w
	}
	updater, ok := p.(MessageUpdater)
	if !ok {
		slog.Debug("progress writer disabled: platform has no MessageUpdater", "platform", p.Name(), "style", w.style)
		return w
	}
	w.enabled = true
	w.updater = updater
	if starter, ok := p.(PreviewStarter); ok {
		w.starter = starter
	}
	if w.style == progressStyleCard {
		if progressCardPayloadForTarget(p, replyCtx) {
			w.usePayload = true
		}
	}
	slog.Debug("progress writer enabled", "platform", p.Name(), "style", w.style, "use_payload", w.usePayload)
	return w
}

func normalizeProgressAgentLabel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "agent":
		return "Agent"
	case "codex":
		return "Codex"
	case "claudecode", "claude-code", "cc":
		return "CC"
	case "gemini":
		return "Gemini"
	case "cursor":
		return "Cursor"
	case "qoder":
		return "Qoder"
	case "iflow":
		return "iFlow"
	case "opencode":
		return "OpenCode"
	case "pi":
		return "PI"
	default:
		n := strings.TrimSpace(name)
		if n == "" {
			return "Agent"
		}
		return strings.ToUpper(n[:1]) + n[1:]
	}
}

// Append appends one progress item and updates the in-place message.
// Returns true when compact rendering handled this item; false means caller
// should fallback to legacy per-event send.
func (w *compactProgressWriter) Append(item string) bool {
	return w.AppendEvent(ProgressEntryInfo, item, "", item)
}

// AppendEvent appends one typed progress event and updates the in-place message.
// fallback is used for compact/plain rendering when style-specific rendering is not available.
func (w *compactProgressWriter) AppendEvent(kind ProgressCardEntryKind, text string, tool string, fallback string) bool {
	return w.AppendStructured(ProgressCardEntry{
		Kind: kind,
		Text: text,
		Tool: tool,
	}, fallback)
}

// AppendStructured appends one structured progress event and updates the in-place message.
func (w *compactProgressWriter) AppendStructured(item ProgressCardEntry, fallback string) bool {
	w.mu.Lock()
	if !w.enabled || w.failed || w.closed {
		w.mu.Unlock()
		return false
	}
	text := strings.TrimSpace(item.Text)
	fallback = strings.TrimSpace(fallback)
	if text == "" && fallback == "" {
		w.mu.Unlock()
		return true
	}
	if text == "" {
		text = fallback
	}
	if fallback == "" {
		fallback = text
	}
	switch item.Kind {
	case ProgressEntryThinking, ProgressEntryError, ProgressEntryInfo:
		if w.transform != nil {
			text = w.transform(text)
			fallback = w.transform(fallback)
		}
	}
	kind := item.Kind
	if kind == "" {
		kind = ProgressEntryInfo
	}
	item.Kind = kind
	item.Text = text
	item.Tool = strings.TrimSpace(item.Tool)
	item.Status = strings.TrimSpace(item.Status)
	w.counts.add(kind)

	switch w.style {
	case progressStyleCard:
		w.items = append(w.items, item)
		w.entries = append(w.entries, fallback)
		var truncated bool
		w.items, w.entries, truncated = trimProgressCardEntriesByLane(w.items, w.entries, w.maxEntries)
		w.truncated = w.truncated || truncated
		if !w.rebuildCardContentLocked() {
			slog.Warn("progress writer: failed to build structured payload", "platform", w.platform.Name())
			w.markFailedLocked()
			w.mu.Unlock()
			return false
		}
	default:
		if w.content == "" {
			w.content = fallback
		} else {
			w.content += "\n\n" + fallback
		}
		w.content = trimCompactProgressText(w.content, compactProgressMaxChars)
	}

	handled, observer, handle := w.flushLocked(false)
	w.mu.Unlock()
	if observer != nil {
		observer(handle)
	}
	return handled
}

// SetHandleObserver registers a callback for the platform handle backing the
// progress card. The callback is invoked synchronously and at most once per
// registration, including immediately when the card already exists.
func (w *compactProgressWriter) SetHandleObserver(observer func(any)) {
	w.mu.Lock()
	w.handleObserver = observer
	callback, handle := w.takeHandleObserverLocked()
	w.mu.Unlock()
	if callback != nil {
		callback(handle)
	}
}

func (w *compactProgressWriter) takeHandleObserverLocked() (func(any), any) {
	if w.handleObserver == nil || w.handle == nil {
		return nil, nil
	}
	observer := w.handleObserver
	w.handleObserver = nil
	return observer, w.handle
}

// SetControls updates the running progress card with an optional hint and
// action buttons. Controls are never retained on a terminal card.
func (w *compactProgressWriter) SetControls(hint string, buttons []CardButton) bool {
	w.mu.Lock()
	if !w.enabled || w.failed || w.closed || w.style != progressStyleCard || !w.usePayload || w.state != ProgressCardStateRunning {
		w.mu.Unlock()
		return false
	}
	w.hint = strings.TrimSpace(hint)
	w.buttons = cloneProgressCardButtons(buttons)
	if len(w.items) == 0 {
		w.mu.Unlock()
		return true
	}
	if !w.rebuildCardContentLocked() {
		w.markFailedLocked()
		w.mu.Unlock()
		return false
	}
	handled, observer, handle := w.flushLocked(true)
	w.mu.Unlock()
	if observer != nil {
		observer(handle)
	}
	return handled
}

// EnableHealthMonitoring lets an authoritative turn monitor keep updating a
// running card after the foreground event stream has disconnected.
func (w *compactProgressWriter) EnableHealthMonitoring() {
	w.mu.Lock()
	w.healthMonitoring = true
	w.mu.Unlock()
}

func (w *compactProgressWriter) DisableHealthMonitoring() {
	w.mu.Lock()
	w.healthMonitoring = false
	w.mu.Unlock()
}

// SetHealth updates only the observable liveness metadata of a running card.
func (w *compactProgressWriter) SetHealth(health ProgressCardHealth, lastVerifiedAt time.Time) bool {
	w.mu.Lock()
	if !w.enabled || w.failed || w.style != progressStyleCard || !w.usePayload || w.state != ProgressCardStateRunning || w.handle == nil {
		w.mu.Unlock()
		return false
	}
	w.health = health
	w.lastVerifiedAt = lastVerifiedAt
	if !w.rebuildCardContentLocked() {
		w.mu.Unlock()
		return false
	}
	handled, observer, handle := w.flushLocked(true)
	w.mu.Unlock()
	if observer != nil {
		observer(handle)
	}
	return handled
}

// Finalize updates card progress state (running/completed/failed) without
// appending a new progress entry.
func (w *compactProgressWriter) Finalize(state ProgressCardState) bool {
	w.mu.Lock()
	if !w.enabled || w.failed || w.closed || w.style != progressStyleCard || !w.usePayload || w.handle == nil {
		w.stopTimersLocked()
		w.closed = true
		w.mu.Unlock()
		return false
	}
	if state == "" {
		state = ProgressCardStateCompleted
	}
	w.state = state
	w.hint = ""
	w.buttons = nil
	if !w.rebuildCardContentLocked() {
		w.stopTimersLocked()
		w.closed = true
		w.mu.Unlock()
		return false
	}
	handled, observer, handle := w.flushLocked(true)
	w.stopTimersLocked()
	w.closed = true
	w.mu.Unlock()
	if observer != nil {
		observer(handle)
	}
	return handled
}

// Close stops background refreshes without changing the card's visible state.
func (w *compactProgressWriter) Close() {
	w.mu.Lock()
	if w.healthMonitoring && w.state == ProgressCardStateRunning {
		w.stopTimersLocked()
		w.mu.Unlock()
		return
	}
	w.stopped = true
	w.stopTimersLocked()
	w.closed = true
	w.mu.Unlock()
}

func (w *compactProgressWriter) rebuildCardContentLocked() bool {
	if len(w.items) == 0 {
		return false
	}
	if w.usePayload {
		elapsedSeconds := int64(time.Since(w.startedAt) / time.Second)
		w.content = buildProgressCardPayloadV2WithMeta(
			w.items, w.truncated, w.agentName, w.lang, w.state, w.hint, w.buttons,
			w.counts, elapsedSeconds, w.health, w.lastVerifiedAt.Unix(),
		)
	} else {
		w.content = renderCardProgressMarkdownFallback(w.entries, w.truncated)
		w.content = trimCompactProgressText(w.content, compactProgressMaxChars)
	}
	return w.content != ""
}

// flushLocked writes the latest complete frame. It returns a deferred handle
// observer because callbacks may re-enter the writer to install controls.
func (w *compactProgressWriter) flushLocked(force bool) (bool, func(any), any) {
	if !w.enabled || w.failed || w.closed {
		return false, nil, nil
	}
	if w.content == "" {
		return true, nil, nil
	}
	if force {
		w.cancelFlushLocked()
	}
	if w.content == w.lastSent {
		w.scheduleHeartbeatLocked()
		return true, nil, nil
	}
	// Once card-style progress has been selected, transport failures must not
	// make individual tool events fall back to standalone chat messages. Keep
	// buffering while the delayed retry catches the card up to the latest state.
	if w.degraded && !force {
		return true, nil, nil
	}

	if w.handle == nil {
		if w.starter != nil {
			callCtx, cancel := w.withAPITimeout()
			handle, err := w.starter.SendPreviewStart(callCtx, w.replyCtx, w.content)
			cancel()
			if err != nil || handle == nil {
				slog.Warn("progress writer: SendPreviewStart failed", "platform", w.platform.Name(), "style", w.style, "error", err, "handle_nil", handle == nil)
				if w.style == progressStyleCard {
					w.scheduleRetryLocked()
					return true, nil, nil
				}
				w.markFailedLocked()
				return false, nil, nil
			}
			w.handle = handle
		} else {
			callCtx, cancel := w.withAPITimeout()
			err := w.platform.Send(callCtx, w.replyCtx, w.content)
			cancel()
			if err != nil {
				slog.Warn("progress writer: initial Send failed", "platform", w.platform.Name(), "style", w.style, "error", err)
				if w.style == progressStyleCard {
					w.scheduleRetryLocked()
					return true, nil, nil
				}
				w.markFailedLocked()
				return false, nil, nil
			}
			w.handle = w.replyCtx
		}
		w.recordSuccessfulUpdateLocked()
		observer, handle := w.takeHandleObserverLocked()
		return true, observer, handle
	}

	if !force && w.minUpdateInterval > 0 {
		elapsed := time.Since(w.lastUpdateAt)
		if elapsed < w.minUpdateInterval {
			w.scheduleFlushLocked(w.minUpdateInterval - elapsed)
			return true, nil, nil
		}
	}

	callCtx, cancel := w.withAPITimeout()
	err := w.updater.UpdateMessage(callCtx, w.handle, w.content)
	cancel()
	if err != nil {
		slog.Warn("progress writer: UpdateMessage failed", "platform", w.platform.Name(), "style", w.style, "error", err)
		if w.style == progressStyleCard {
			w.scheduleRetryLocked()
			return true, nil, nil
		}
		w.markFailedLocked()
		return false, nil, nil
	}
	w.recordSuccessfulUpdateLocked()
	return true, nil, nil
}

func trimProgressCardEntriesByLane(items []ProgressCardEntry, entries []string, perLaneLimit int) ([]ProgressCardEntry, []string, bool) {
	if perLaneLimit <= 0 || len(items) == 0 {
		return items, entries, false
	}

	const (
		reasoningLane = iota
		toolLane
		otherLane
	)
	laneFor := func(kind ProgressCardEntryKind) int {
		switch kind {
		case ProgressEntryThinking:
			return reasoningLane
		case ProgressEntryToolUse, ProgressEntryToolResult:
			return toolLane
		default:
			return otherLane
		}
	}

	keep := make([]bool, len(items))
	var counts [3]int
	truncated := false
	for i := len(items) - 1; i >= 0; i-- {
		lane := laneFor(items[i].Kind)
		if counts[lane] >= perLaneLimit {
			truncated = true
			continue
		}
		counts[lane]++
		keep[i] = true
	}
	if !truncated {
		return items, entries, false
	}

	trimmedItems := make([]ProgressCardEntry, 0, counts[reasoningLane]+counts[toolLane]+counts[otherLane])
	trimmedEntries := make([]string, 0, cap(trimmedItems))
	for i, item := range items {
		if !keep[i] {
			continue
		}
		trimmedItems = append(trimmedItems, item)
		if i < len(entries) {
			trimmedEntries = append(trimmedEntries, entries[i])
		}
	}
	return trimmedItems, trimmedEntries, true
}

func (w *compactProgressWriter) recordSuccessfulUpdateLocked() {
	w.lastSent = w.content
	w.lastUpdateAt = time.Now()
	w.degraded = false
	w.scheduleHeartbeatLocked()
}

func (w *compactProgressWriter) scheduleFlushLocked(delay time.Duration) {
	if w.flushTimer != nil || w.closed || w.failed {
		return
	}
	if delay < 0 {
		delay = 0
	}
	w.flushTimer = time.AfterFunc(delay, w.flushPending)
}

func (w *compactProgressWriter) flushPending() {
	w.mu.Lock()
	w.flushTimer = nil
	if w.ctx.Err() != nil || w.closed || w.failed {
		w.mu.Unlock()
		return
	}
	handled, observer, handle := w.flushLocked(false)
	w.mu.Unlock()
	if !handled {
		return
	}
	if observer != nil {
		observer(handle)
	}
}

func (w *compactProgressWriter) scheduleHeartbeatLocked() {
	if w.heartbeatInterval <= 0 || w.style != progressStyleCard || !w.usePayload ||
		w.state != ProgressCardStateRunning || w.handle == nil || w.closed || w.failed {
		return
	}
	if w.heartbeatTimer != nil {
		w.heartbeatTimer.Stop()
	}
	w.heartbeatTimer = time.AfterFunc(w.heartbeatInterval, w.flushHeartbeat)
}

func (w *compactProgressWriter) flushHeartbeat() {
	w.mu.Lock()
	w.heartbeatTimer = nil
	if w.ctx.Err() != nil || w.closed || w.failed || w.state != ProgressCardStateRunning {
		w.mu.Unlock()
		return
	}
	if !w.rebuildCardContentLocked() {
		w.mu.Unlock()
		return
	}
	handled, observer, handle := w.flushLocked(true)
	w.mu.Unlock()
	if !handled {
		return
	}
	if observer != nil {
		observer(handle)
	}
}

func (w *compactProgressWriter) cancelFlushLocked() {
	if w.flushTimer != nil {
		w.flushTimer.Stop()
		w.flushTimer = nil
	}
}

func (w *compactProgressWriter) stopTimersLocked() {
	w.cancelFlushLocked()
	if w.heartbeatTimer != nil {
		w.heartbeatTimer.Stop()
		w.heartbeatTimer = nil
	}
	w.stopRetryLocked()
}

func (w *compactProgressWriter) markFailedLocked() {
	w.failed = true
	w.stopTimersLocked()
}

// Stop cancels any delayed retry owned by this turn.
func (w *compactProgressWriter) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.closed = true
	w.stopTimersLocked()
}

func (w *compactProgressWriter) scheduleRetryLocked() {
	w.degraded = true
	if w.stopped || w.closed || w.retryTimer != nil {
		return
	}
	delay := w.nextRetryDelay
	if delay <= 0 {
		delay = progressRetryInitialDelay
	}
	w.retryTimer = time.AfterFunc(delay, w.retry)
}

func (w *compactProgressWriter) stopRetryLocked() {
	if w.retryTimer != nil {
		w.retryTimer.Stop()
		w.retryTimer = nil
	}
}

func (w *compactProgressWriter) retry() {
	w.mu.Lock()
	w.retryTimer = nil
	if w.stopped || w.closed || !w.enabled || w.failed || !w.degraded || w.content == "" {
		w.mu.Unlock()
		return
	}
	select {
	case <-w.ctx.Done():
		w.stopped = true
		w.mu.Unlock()
		return
	default:
	}

	callCtx, cancel := w.withAPITimeout()
	var err error
	if w.handle == nil {
		if w.starter != nil {
			var handle any
			handle, err = w.starter.SendPreviewStart(callCtx, w.replyCtx, w.content)
			if err == nil && handle == nil {
				err = errors.New("preview start returned nil handle")
			}
			if err == nil {
				w.handle = handle
			}
		} else {
			err = w.platform.Send(callCtx, w.replyCtx, w.content)
			if err == nil {
				w.handle = w.replyCtx
			}
		}
	} else {
		err = w.updater.UpdateMessage(callCtx, w.handle, w.content)
	}
	cancel()
	if err != nil {
		slog.Warn("progress writer: delayed retry failed", "platform", w.platform.Name(), "style", w.style, "retry_delay", w.nextRetryDelay, "error", err)
		w.nextRetryDelay *= 2
		if w.nextRetryDelay > progressRetryMaxDelay {
			w.nextRetryDelay = progressRetryMaxDelay
		}
		w.scheduleRetryLocked()
		w.mu.Unlock()
		return
	}

	w.recordSuccessfulUpdateLocked()
	w.nextRetryDelay = progressRetryInitialDelay
	observer, handle := w.takeHandleObserverLocked()
	w.mu.Unlock()
	slog.Info("progress writer: delayed retry recovered", "platform", w.platform.Name(), "style", w.style)
	if observer != nil {
		observer(handle)
	}
}

func (w *compactProgressWriter) withAPITimeout() (context.Context, context.CancelFunc) {
	if _, hasDeadline := w.ctx.Deadline(); hasDeadline {
		return w.ctx, func() {}
	}
	return context.WithTimeout(w.ctx, compactProgressAPITimeout)
}

func renderCardProgressMarkdownFallback(entries []string, truncated bool) string {
	var b strings.Builder
	b.WriteString("⏳ **Progress**\n")
	if truncated {
		b.WriteString("_Showing latest updates only._\n")
	}
	for i, entry := range entries {
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(strings.ReplaceAll(entry, "\n", "\n   "))
	}
	return b.String()
}

func trimCompactProgressText(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	s = strings.TrimPrefix(s, "…\n")
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	rs := []rune(s)
	tail := strings.TrimLeft(string(rs[len(rs)-maxRunes:]), "\n")
	return "…\n" + tail
}
