package codex

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const appServerConversationReadTimeout = 2 * time.Second
const appServerConversationEventBuffer = 256

// SupportsConversationClientMarker reports the daemon-only marker round trip
// used to distinguish cc-connect foreground turns from external turns.
func (a *Agent) SupportsConversationClientMarker() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.backend == "app_server" && a.appServerTransport == appServerTransportDaemon
}

type appServerConversationThread struct {
	ID     string `json:"id"`
	Cwd    string `json:"cwd"`
	Status struct {
		Type        string   `json:"type"`
		ActiveFlags []string `json:"activeFlags"`
	} `json:"status"`
	Turns []appServerConversationTurn `json:"turns"`
}

type appServerConversationTurn struct {
	ID          string                      `json:"id"`
	Status      string                      `json:"status"`
	StartedAt   int64                       `json:"startedAt"`
	CompletedAt int64                       `json:"completedAt"`
	Items       []map[string]any            `json:"items"`
	Error       *appServerConversationError `json:"error"`
}

type appServerConversationError struct {
	Message string `json:"message"`
}

type appServerThreadReadResponse struct {
	Thread appServerConversationThread `json:"thread"`
}

type appServerTurnsListResponse struct {
	Data       []appServerConversationTurn `json:"data"`
	NextCursor *string                     `json:"nextCursor"`
}

type appServerTurnSteerResponse struct {
	TurnID string `json:"turnId"`
}

// GetConversation returns Codex's own persisted view of a thread. Shared
// daemon sessions are queried through an initialize-only app-server
// connection; other backends fall back to Codex's JSONL transcript.
func (a *Agent) GetConversation(ctx context.Context, sessionID string, limit int) (*core.ConversationSnapshot, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("codex: conversation session id is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	a.mu.RLock()
	backend := a.backend
	transport := a.appServerTransport
	socket := a.appServerSocket
	codexHome := a.codexHome
	workDir := a.workDir
	extraEnv := append([]string(nil), a.configEnv...)
	extraEnv = append(extraEnv, a.sessionEnv...)
	a.mu.RUnlock()

	if backend != "app_server" || transport != appServerTransportDaemon {
		entries, err := getSessionHistory(sessionID, codexHome, 0)
		if err != nil {
			return nil, err
		}
		return conversationFromHistory(sessionID, entries, limit), nil
	}

	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	client, err := a.ensureConversationClientLocked(socket, codexHome, workDir, extraEnv)
	if err != nil {
		return nil, err
	}

	snapshot, err := readAppServerConversation(client, sessionID, workDir, limit)
	if err != nil && !client.Alive() {
		_ = client.Close()
		a.conversationClient = nil
	}
	return snapshot, err
}

// GetConversationWindow pages backwards until watermark is covered, then
// returns that turn and every newer turn in chronological order.
func (a *Agent) GetConversationWindow(ctx context.Context, sessionID, watermark string, maxTurns int) (*core.ConversationSnapshot, bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	watermark = strings.TrimSpace(watermark)
	if sessionID == "" {
		return nil, false, fmt.Errorf("codex: conversation session id is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	a.mu.RLock()
	backend := a.backend
	transport := a.appServerTransport
	socket := a.appServerSocket
	codexHome := a.codexHome
	workDir := a.workDir
	extraEnv := append([]string(nil), a.configEnv...)
	extraEnv = append(extraEnv, a.sessionEnv...)
	a.mu.RUnlock()
	if backend != "app_server" || transport != appServerTransportDaemon {
		snapshot, err := a.GetConversation(ctx, sessionID, maxTurns)
		return snapshot, conversationSnapshotContains(snapshot, watermark), err
	}

	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	client, err := a.ensureConversationClientLocked(socket, codexHome, workDir, extraEnv)
	if err != nil {
		return nil, false, err
	}
	snapshot, covered, err := readAppServerConversationWindow(client, sessionID, workDir, watermark, maxTurns)
	if err != nil && !client.Alive() {
		_ = client.Close()
		a.conversationClient = nil
	}
	return snapshot, covered, err
}

func conversationSnapshotContains(snapshot *core.ConversationSnapshot, turnID string) bool {
	if strings.TrimSpace(turnID) == "" {
		return true
	}
	if snapshot == nil {
		return false
	}
	for _, turn := range snapshot.Turns {
		if turn.ID == turnID {
			return true
		}
	}
	return false
}

// InterruptConversationTurn interrupts only the exact daemon turn selected by
// an authorized tracking card. It never falls back to a current/latest turn.
func (a *Agent) InterruptConversationTurn(ctx context.Context, sessionID, turnID string) error {
	sessionID = strings.TrimSpace(sessionID)
	turnID = strings.TrimSpace(turnID)
	if sessionID == "" || turnID == "" {
		return fmt.Errorf("codex: interrupt requires thread and turn ids")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	a.mu.RLock()
	backend := a.backend
	transport := a.appServerTransport
	socket := a.appServerSocket
	codexHome := a.codexHome
	workDir := a.workDir
	extraEnv := append([]string(nil), a.configEnv...)
	extraEnv = append(extraEnv, a.sessionEnv...)
	a.mu.RUnlock()
	if backend != "app_server" || transport != appServerTransportDaemon {
		return fmt.Errorf("codex: authoritative turn interruption requires daemon transport")
	}

	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	client, err := a.ensureConversationClientLocked(socket, codexHome, workDir, extraEnv)
	if err != nil {
		return err
	}
	if err := client.requestWithTimeout("turn/interrupt", map[string]any{
		"threadId": sessionID,
		"turnId":   turnID,
	}, nil, appServerConversationReadTimeout); err != nil {
		if !client.Alive() {
			_ = client.Close()
			a.conversationClient = nil
		}
		return fmt.Errorf("codex: interrupt turn %q: %w", turnID, err)
	}
	return nil
}

// SteerConversationTurn appends input only to expectedTurnID. It uses a
// short-lived initialize-only control connection and never resumes the thread
// or falls back to turn/start when the active turn changed.
func (a *Agent) SteerConversationTurn(ctx context.Context, sessionID, expectedTurnID, input, clientUserMessageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	sessionID = strings.TrimSpace(sessionID)
	expectedTurnID = strings.TrimSpace(expectedTurnID)
	if sessionID == "" || expectedTurnID == "" {
		return fmt.Errorf("codex: steer requires thread and expected turn ids")
	}
	if strings.TrimSpace(input) == "" && len(images) == 0 && len(files) == 0 {
		return fmt.Errorf("codex: steer input is empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	a.mu.RLock()
	backend := a.backend
	transport := a.appServerTransport
	socket := a.appServerSocket
	codexHome := a.codexHome
	workDir := a.workDir
	extraEnv := append([]string(nil), a.configEnv...)
	extraEnv = append(extraEnv, a.sessionEnv...)
	a.mu.RUnlock()
	if backend != "app_server" || transport != appServerTransportDaemon {
		return fmt.Errorf("codex: exact turn steering requires daemon transport")
	}

	resolvedSocket, err := resolveAppServerSocket(socket, codexHome, extraEnv)
	if err != nil {
		return fmt.Errorf("codex: resolve steer socket: %w", err)
	}
	client, err := newAppServerConversationClient(resolvedSocket, workDir)
	if err != nil {
		return err
	}
	defer client.Close()
	if _, err := readAppServerConversation(client, sessionID, workDir, 1); err != nil {
		return err
	}
	if err := client.bindReadOnlyThread(sessionID); err != nil {
		return err
	}

	prompt := input
	if len(files) > 0 {
		paths := core.SaveFilesToDisk(workDir, clientUserMessageID, files)
		prompt = core.AppendFileRefs(prompt, paths)
	}
	prompt, imagePaths, err := client.stageImages(prompt, images)
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, 1+len(imagePaths))
	items = append(items, map[string]any{"type": "text", "text": prompt, "text_elements": []any{}})
	for _, path := range imagePaths {
		items = append(items, map[string]any{"type": "localImage", "path": path})
	}
	params := map[string]any{
		"threadId": sessionID, "expectedTurnId": expectedTurnID, "input": items,
	}
	if marker := strings.TrimSpace(clientUserMessageID); marker != "" {
		params["clientUserMessageId"] = marker
	}
	timeout := appServerConversationReadTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return context.DeadlineExceeded
	}
	var response appServerTurnSteerResponse
	if err := client.requestWithTimeout("turn/steer", params, &response, timeout); err != nil {
		return fmt.Errorf("codex: steer turn %q: %w", expectedTurnID, err)
	}
	if response.TurnID != expectedTurnID {
		return fmt.Errorf("codex: steer accepted unexpected turn %q (expected %q)", response.TurnID, expectedTurnID)
	}
	return nil
}

type appServerConversationObserver struct {
	events <-chan core.Event
	client *appServerSession
}

func (o *appServerConversationObserver) Events() <-chan core.Event {
	return o.events
}

func (o *appServerConversationObserver) RespondPermission(requestID string, result core.PermissionResult) error {
	if o == nil || o.client == nil {
		return fmt.Errorf("codex: conversation observer is closed")
	}
	return o.client.RespondPermission(requestID, result)
}

// WatchConversation opens an initialize-only, passive daemon connection and
// scopes notifications to sessionID. Interactive mirrors should use
// OpenConversationObserver so the daemon also routes blocking questions.
func (a *Agent) WatchConversation(ctx context.Context, sessionID string) (<-chan core.Event, error) {
	observer, err := a.openConversationObserver(ctx, sessionID, false)
	if err != nil {
		return nil, err
	}
	return observer.Events(), nil
}

// OpenConversationObserver rejoins the thread on a lease-free connection.
// Codex app-server broadcasts the same server request and request ID to every
// resumed client; the first response wins and serverRequest/resolved makes the
// remaining clients stale.
func (a *Agent) OpenConversationObserver(ctx context.Context, sessionID string) (core.ConversationObserver, error) {
	return a.openConversationObserver(ctx, sessionID, true)
}

func (a *Agent) openConversationObserver(ctx context.Context, sessionID string, interactive bool) (*appServerConversationObserver, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("codex: conversation observer session id is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	a.mu.RLock()
	backend := a.backend
	transport := a.appServerTransport
	socket := a.appServerSocket
	codexHome := a.codexHome
	workDir := a.workDir
	extraEnv := append([]string(nil), a.configEnv...)
	extraEnv = append(extraEnv, a.sessionEnv...)
	a.mu.RUnlock()
	if backend != "app_server" || transport != appServerTransportDaemon {
		return nil, core.ErrNotSupported
	}

	resolvedSocket, err := resolveAppServerSocket(socket, codexHome, extraEnv)
	if err != nil {
		return nil, fmt.Errorf("codex: resolve observer socket: %w", err)
	}
	client, err := newAppServerConversationObserverClient(resolvedSocket, workDir, appServerConversationEventBuffer)
	if err != nil {
		return nil, err
	}
	client.observerInput = interactive
	// Validate identity and workspace on this exact connection before allowing
	// it to relay any notification to core.
	if _, err := readAppServerConversation(client, sessionID, workDir, 1); err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := client.bindReadOnlyThread(sessionID); err != nil {
		_ = client.Close()
		return nil, err
	}
	if interactive {
		var resumed threadResumeResponse
		if err := client.requestWithTimeout("thread/resume", map[string]any{
			"threadId":     sessionID,
			"excludeTurns": true,
		}, &resumed, appServerConversationReadTimeout); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("codex: subscribe conversation observer: %w", err)
		}
		if resumed.Thread.ID != sessionID {
			_ = client.Close()
			return nil, fmt.Errorf("codex: observer resumed thread %q, want %q", resumed.Thread.ID, sessionID)
		}
	}
	// Progress notifications only wake a snapshot reconciliation, so one
	// queued wakeup is sufficient. Keeping this relay bounded prevents token
	// deltas from delaying a blocking question or terminal event behind
	// hundreds of stale refreshes.
	output := make(chan core.Event, 1)
	go func() {
		defer close(output)
		defer client.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case <-client.ctx.Done():
				return
			case event, ok := <-client.Events():
				if !ok {
					return
				}
				if appServerObserverEventRequiresDelivery(event) {
					select {
					case output <- event:
					case <-ctx.Done():
						return
					}
				} else {
					select {
					case output <- event:
					default:
					}
				}
				if event.Type == core.EventError {
					return
				}
			}
		}
	}()
	return &appServerConversationObserver{events: output, client: client}, nil
}

// ensureConversationClientLocked returns the initialize-only daemon control
// connection used for snapshots and explicit tracked-turn interruption.
// a.conversationMu must be held by the caller.
func (a *Agent) ensureConversationClientLocked(socket, codexHome, workDir string, extraEnv []string) (*appServerSession, error) {
	if a.conversationClient != nil && a.conversationClient.Alive() {
		return a.conversationClient, nil
	}
	if a.conversationClient != nil {
		_ = a.conversationClient.Close()
		a.conversationClient = nil
	}
	resolvedSocket, err := resolveAppServerSocket(socket, codexHome, extraEnv)
	if err != nil {
		return nil, fmt.Errorf("codex: resolve conversation socket: %w", err)
	}
	client, err := newAppServerConversationClient(resolvedSocket, workDir)
	if err != nil {
		return nil, err
	}
	a.conversationClient = client
	return client, nil
}

func newAppServerConversationClient(socketPath, workDir string) (*appServerSession, error) {
	return newAppServerConversationClientWithBuffer(socketPath, workDir, 1)
}

func newAppServerConversationClientWithBuffer(socketPath, workDir string, eventBuffer int) (*appServerSession, error) {
	return newAppServerConversationClientWithOptions(socketPath, workDir, eventBuffer, false)
}

func newAppServerConversationObserverClient(socketPath, workDir string, eventBuffer int) (*appServerSession, error) {
	return newAppServerConversationClientWithOptions(socketPath, workDir, eventBuffer, true)
}

func newAppServerConversationClientWithOptions(socketPath, workDir string, eventBuffer int, observerProgress bool) (*appServerSession, error) {
	if eventBuffer < 1 {
		eventBuffer = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &appServerSession{
		transport:        appServerTransportDaemon,
		socketPath:       strings.TrimSpace(socketPath),
		workDir:          workDir,
		events:           make(chan core.Event, eventBuffer),
		ctx:              ctx,
		cancel:           cancel,
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingMethods:   make(map[int64]string),
		pendingApprovals: make(map[string]chan core.PermissionResult),
		observedInputs:   make(map[string]appServerObservedRequestUserInput),
		permissionDone:   make(chan string, 32),
		observerProgress: observerProgress,
	}
	s.alive.Store(true)
	if err := s.connectDaemon(); err != nil {
		cancel()
		return nil, fmt.Errorf("codex: connect conversation reader: %w", err)
	}
	if err := s.initialize(); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("codex: initialize conversation reader: %w", err)
	}
	return s, nil
}

func readAppServerConversation(client *appServerSession, sessionID, workDir string, limit int) (*core.ConversationSnapshot, error) {
	snapshot, _, err := readAppServerConversationWindow(client, sessionID, workDir, "", limit)
	return snapshot, err
}

func readAppServerConversationWindow(client *appServerSession, sessionID, workDir, watermark string, maxTurns int) (*core.ConversationSnapshot, bool, error) {
	const defaultMaxTurns = 512
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	watermark = strings.TrimSpace(watermark)
	var readResp appServerThreadReadResponse
	if err := client.requestWithTimeout("thread/read", map[string]any{
		"threadId": sessionID, "includeTurns": false,
	}, &readResp, appServerConversationReadTimeout); err != nil {
		return nil, false, fmt.Errorf("codex: read thread %q: %w", sessionID, err)
	}
	if err := validateConversationThread(readResp.Thread, sessionID, workDir); err != nil {
		return nil, false, err
	}

	resultsDescending := true
	covered := watermark == ""
	var turnData []appServerConversationTurn
	var cursor string
	var turnsErr error
	for len(turnData) < maxTurns {
		remaining := maxTurns - len(turnData)
		pageLimit := min(10, remaining)
		params := map[string]any{
			"threadId": sessionID, "sortDirection": "desc", "itemsView": "full", "limit": pageLimit,
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page appServerTurnsListResponse
		turnsErr = client.requestWithTimeout("thread/turns/list", params, &page, appServerConversationReadTimeout)
		if turnsErr != nil {
			break
		}
		turnData = append(turnData, page.Data...)
		if watermark != "" {
			for index, turn := range turnData {
				if strings.TrimSpace(turn.ID) == watermark {
					turnData = turnData[:index+1]
					covered = true
					break
				}
			}
			if covered {
				break
			}
		}
		if page.NextCursor == nil || strings.TrimSpace(*page.NextCursor) == "" {
			break
		}
		cursor = strings.TrimSpace(*page.NextCursor)
	}
	if turnsErr != nil {
		// Older app-server versions do not expose thread/turns/list. In that
		// case thread/read remains Codex's authoritative source.
		if !appServerMethodUnavailable(turnsErr) {
			return nil, false, fmt.Errorf("codex: list turns for %q: %w", sessionID, turnsErr)
		}
		var fullResp appServerThreadReadResponse
		if fallbackErr := client.requestWithTimeout("thread/read", map[string]any{
			"threadId":     sessionID,
			"includeTurns": true,
		}, &fullResp, appServerConversationReadTimeout); fallbackErr != nil {
			return nil, false, fmt.Errorf("codex: list turns for %q: %w", sessionID, turnsErr)
		}
		if err := validateConversationThread(fullResp.Thread, sessionID, workDir); err != nil {
			return nil, false, err
		}
		readResp = fullResp
		turnData = fullResp.Thread.Turns
		if len(turnData) > maxTurns {
			turnData = turnData[len(turnData)-maxTurns:]
		}
		covered = watermark == ""
		for _, turn := range turnData {
			if strings.TrimSpace(turn.ID) == watermark {
				covered = true
				break
			}
		}
		resultsDescending = false
	}

	turns := make([]core.ConversationTurn, 0, len(turnData))
	if resultsDescending {
		for i := len(turnData) - 1; i >= 0; i-- {
			turns = append(turns, mapAppServerConversationTurn(turnData[i]))
		}
	} else {
		for _, turn := range turnData {
			turns = append(turns, mapAppServerConversationTurn(turn))
		}
	}
	return &core.ConversationSnapshot{
		SessionID:   sessionID,
		ThreadState: strings.TrimSpace(readResp.Thread.Status.Type),
		ActiveFlags: append([]string(nil), readResp.Thread.Status.ActiveFlags...),
		Turns:       turns,
		RetrievedAt: time.Now(),
	}, covered, nil
}

func appServerMethodUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "method not found") ||
		strings.Contains(message, "unknown method") ||
		strings.Contains(message, "not implemented")
}

func validateConversationThread(thread appServerConversationThread, sessionID, workDir string) error {
	if strings.TrimSpace(thread.ID) != sessionID {
		return fmt.Errorf("codex: thread/read returned %q for requested thread %q", thread.ID, sessionID)
	}
	got := canonicalConversationPath(thread.Cwd)
	want := canonicalConversationPath(workDir)
	if got == "" || want == "" || got != want {
		return fmt.Errorf("codex: refusing thread %q from workspace %q (expected %q)", sessionID, thread.Cwd, workDir)
	}
	return nil
}

func canonicalConversationPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}

func mapAppServerConversationTurn(turn appServerConversationTurn) core.ConversationTurn {
	mapped := core.ConversationTurn{
		ID:          strings.TrimSpace(turn.ID),
		Status:      mapConversationTurnStatus(turn.Status),
		StartedAt:   unixConversationTime(turn.StartedAt),
		CompletedAt: unixConversationTime(turn.CompletedAt),
	}
	for _, item := range turn.Items {
		itemType, _ := item["type"].(string)
		switch itemType {
		case "userMessage":
			if text := appServerUserMessageText(item); text != "" {
				mapped.Messages = append(mapped.Messages, core.ConversationMessage{
					ID: stringMapValue(item, "id"), ClientID: stringMapValue(item, "clientId"), Role: "user", Content: stripCodexPromptPreamble(text),
				})
			}
		case "agentMessage":
			if text := stringMapValue(item, "text"); text != "" {
				mapped.Messages = append(mapped.Messages, core.ConversationMessage{
					ID: stringMapValue(item, "id"), Role: "assistant", Content: text, Phase: stringMapValue(item, "phase"),
				})
			}
		case "plan":
			if text := stringMapValue(item, "text"); text != "" {
				mapped.Messages = append(mapped.Messages, core.ConversationMessage{
					ID: stringMapValue(item, "id"), Role: "assistant", Content: text, Phase: "proposed_plan",
				})
			}
		case "reasoning":
			// Keep reasoning out of history messages. Its user-visible summary is
			// projected through PresentationEvents below, matching the live card.
		default:
			if appServerConversationRequestUserInputItem(itemType, item) {
				continue
			}
			if activity, ok := appServerConversationActivity(itemType, item, mapped.Status); ok {
				mapped.Activities = append(mapped.Activities, activity)
			}
		}
	}
	mapped.PresentationEvents = appServerConversationPresentationEvents(turn.Items, mapped.Status)
	return mapped
}

func appServerConversationPresentationEvents(items []map[string]any, turnStatus core.ConversationTurnStatus) []core.Event {
	var events []core.Event
	var pendingMessages []core.Event
	flushPending := func(eventType core.EventType) {
		for _, event := range pendingMessages {
			event.Type = eventType
			events = append(events, event)
		}
		pendingMessages = pendingMessages[:0]
	}

	for _, item := range items {
		itemType := stringMapValue(item, "type")
		itemID := stringMapValue(item, "id")
		switch itemType {
		case "userMessage", "hookPrompt", "contextCompaction":
			continue
		case "agentMessage", "plan":
			text, _ := item["text"].(string)
			if strings.TrimSpace(text) != "" {
				pendingMessages = append(pendingMessages, core.Event{ItemID: itemID, Content: text})
			}
			continue
		case "reasoning":
			if text := appServerReasoningText(item); text != "" {
				events = append(events, core.Event{Type: core.EventThinking, ItemID: itemID, Content: text})
			}
			continue
		}
		if appServerConversationRequestUserInputItem(itemType, item) {
			// request_user_input has a dedicated interactive card. Preserve the
			// preceding commentary boundary, but never render its transport JSON
			// as an ordinary tool invocation or result in the mirror card.
			flushPending(core.EventThinking)
			continue
		}

		// The live app-server adapter flushes completed agent messages as
		// thinking when the next tool starts. Reproduce that boundary before
		// projecting the authoritative item into the same foreground events.
		flushPending(core.EventThinking)
		if event, ok := appServerToolUseEvent(item); ok {
			event.ItemID = itemID
			events = append(events, event)
		}
		if appServerConversationItemCompleted(item, turnStatus) {
			if event, ok := appServerToolResultEvent(item); ok {
				event.ItemID = itemID
				events = append(events, event)
			}
		}
	}

	if conversationTurnStatusTerminal(turnStatus) {
		flushPending(core.EventText)
	}
	return events
}

func appServerConversationRequestUserInputItem(itemType string, item map[string]any) bool {
	if itemType != "dynamicToolCall" {
		return false
	}
	tool := strings.ToLower(strings.TrimSpace(stringMapValue(item, "tool")))
	tool = strings.NewReplacer("_", "", "-", "", "/", "").Replace(tool)
	return tool == "requestuserinput"
}

func appServerConversationItemCompleted(item map[string]any, turnStatus core.ConversationTurnStatus) bool {
	status := normalizeConversationStatus(stringMapValue(item, "status"))
	if status == "in_progress" {
		return false
	}
	if status != "" {
		return true
	}
	if stringMapValue(item, "type") == "webSearch" {
		return true
	}
	return conversationTurnStatusTerminal(turnStatus)
}

func conversationTurnStatusTerminal(status core.ConversationTurnStatus) bool {
	switch status {
	case core.ConversationTurnCompleted, core.ConversationTurnFailed, core.ConversationTurnInterrupted:
		return true
	default:
		return false
	}
}

func appServerConversationActivity(itemType string, item map[string]any, turnStatus core.ConversationTurnStatus) (core.ConversationActivity, bool) {
	kind := appServerActivityKind(itemType)
	if kind == "" {
		return core.ConversationActivity{}, false
	}
	activity := core.ConversationActivity{
		ID:     stringMapValue(item, "id"),
		Kind:   kind,
		Status: normalizeConversationStatus(stringMapValue(item, "status")),
	}
	if activity.Status == "" {
		activity.Status = string(turnStatus)
	}

	switch itemType {
	case "commandExecution":
		activity.Name = "Bash"
		activity.Summary = stringMapValue(item, "command")
		activity.Result = truncate(stringMapValue(item, "aggregatedOutput"), 4_000)
		if exitCode, ok := toInt(item["exitCode"]); ok {
			activity.ExitCode = &exitCode
		}
	case "mcpToolCall":
		server := stringMapValue(item, "server")
		tool := stringMapValue(item, "tool")
		activity.Name = strings.Trim(strings.Join([]string{server, tool}, ":"), ":")
		activity.Summary = truncate(appServerJSON(item["arguments"]), 4_000)
		activity.Result = truncate(appServerJSON(item["result"]), 4_000)
		if activity.Result == "" {
			activity.Result = truncate(appServerJSON(item["error"]), 4_000)
		}
	case "webSearch":
		activity.Name = "WebSearch"
		activity.Summary = stringMapValue(item, "query")
	case "dynamicToolCall":
		activity.Name = stringMapValue(item, "tool")
		activity.Summary = truncate(appServerJSON(item["arguments"]), 4_000)
		activity.Result = truncate(appServerDynamicToolText(item["contentItems"]), 4_000)
	case "fileChange":
		activity.Name = "Patch"
		activity.Summary = truncate(appServerJSON(item["changes"]), 4_000)
	case "collabAgentToolCall":
		activity.Name = "Agent"
		activity.Summary = truncate(appServerJSON(item), 4_000)
	}
	if activity.Name == "" {
		activity.Name = kind
	}
	if activity.Status != "" && activity.Status != "in_progress" {
		success := appServerToolSuccess(activity.Status, activity.ExitCode)
		activity.Success = &success
	}
	return activity, true
}

func appServerUserMessageText(item map[string]any) string {
	raw, ok := item["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, entry := range raw {
		content, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if kind := stringMapValue(content, "type"); kind != "text" && kind != "input_text" {
			continue
		}
		if text := stringMapValue(content, "text"); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func stripCodexPromptPreamble(prompt string) string {
	const prefix = "Before answering, follow these project-level instructions for this cc-connect session. They are not user content."
	const delimiter = "\n\n---\n\nUser message:\n"
	if strings.HasPrefix(prompt, prefix) {
		if i := strings.Index(prompt, delimiter); i >= 0 {
			return strings.TrimSpace(prompt[i+len(delimiter):])
		}
	}
	return strings.TrimSpace(prompt)
}

func appServerActivityKind(itemType string) string {
	switch itemType {
	case "commandExecution":
		return "shell"
	case "mcpToolCall":
		return "mcp"
	case "webSearch":
		return "web_search"
	case "dynamicToolCall":
		return "tool"
	case "fileChange":
		return "file_change"
	case "collabAgentToolCall":
		return "agent"
	default:
		return ""
	}
}

func stringMapValue(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func mapConversationTurnStatus(status string) core.ConversationTurnStatus {
	switch normalizeConversationStatus(status) {
	case "in_progress":
		return core.ConversationTurnInProgress
	case "completed":
		return core.ConversationTurnCompleted
	case "failed":
		return core.ConversationTurnFailed
	case "interrupted":
		return core.ConversationTurnInterrupted
	default:
		return core.ConversationTurnUnknown
	}
}

func normalizeConversationStatus(status string) string {
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

func unixConversationTime(seconds int64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func conversationFromHistory(sessionID string, entries []core.HistoryEntry, limit int) *core.ConversationSnapshot {
	var turns []core.ConversationTurn
	for _, entry := range entries {
		if entry.Role == "user" || len(turns) == 0 {
			turns = append(turns, core.ConversationTurn{
				ID:        fmt.Sprintf("jsonl-%d", len(turns)+1),
				Status:    core.ConversationTurnCompleted,
				StartedAt: entry.Timestamp,
			})
		}
		turn := &turns[len(turns)-1]
		turn.Messages = append(turn.Messages, core.ConversationMessage{
			Role: entry.Role, Content: stripCodexPromptPreamble(entry.Content), Phase: historyMessagePhase(entry.Role),
		})
		turn.CompletedAt = entry.Timestamp
	}
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return &core.ConversationSnapshot{
		SessionID: sessionID, ThreadState: "idle", Turns: turns, RetrievedAt: time.Now(),
	}
}

func historyMessagePhase(role string) string {
	if role == "assistant" {
		return "final_answer"
	}
	return ""
}
