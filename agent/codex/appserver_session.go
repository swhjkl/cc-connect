package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

type rpcResponseEnvelope struct {
	ID     any             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcNotificationEnvelope struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initResponse struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type threadStartResponse struct {
	Cwd             string  `json:"cwd"`
	Model           string  `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
	Thread          struct {
		ID string `json:"id"`
	} `json:"thread"`
}

type threadResumeResponse struct {
	Cwd             string  `json:"cwd"`
	Model           string  `json:"model"`
	ReasoningEffort *string `json:"reasoningEffort"`
	Thread          struct {
		ID string `json:"id"`
	} `json:"thread"`
}

type turnStartResponse struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}

type turnNotification struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
}

type itemNotification struct {
	ThreadID string         `json:"threadId"`
	TurnID   string         `json:"turnId"`
	Item     map[string]any `json:"item"`
}

type errorNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Message  string `json:"message"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
}

type appServerThreadIdentity struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type appServerBufferedNotification struct {
	method string
	params json.RawMessage
}

type appServerDaemonWriterRegistry struct {
	mu     sync.Mutex
	owners map[string]*appServerSession
}

type appServerRateLimitsResponse struct {
	RateLimits          appServerRateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID map[string]appServerRateLimitSnapshot `json:"rateLimitsByLimitId"`
}

type appServerRateLimitSnapshot struct {
	LimitID   string                    `json:"limitId"`
	LimitName string                    `json:"limitName"`
	PlanType  string                    `json:"planType"`
	Primary   *appServerRateLimitWindow `json:"primary"`
	Secondary *appServerRateLimitWindow `json:"secondary"`
	Credits   *appServerCreditsSnapshot `json:"credits"`
}

type appServerRateLimitWindow struct {
	UsedPercent        int   `json:"usedPercent"`
	WindowDurationMins int   `json:"windowDurationMins"`
	ResetsAt           int64 `json:"resetsAt"`
}

type appServerCreditsSnapshot struct {
	Balance    *string `json:"balance"`
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
}

type appServerRequestUserInputParams struct {
	ThreadID  string                              `json:"threadId"`
	TurnID    string                              `json:"turnId"`
	ItemID    string                              `json:"itemId"`
	Questions []appServerRequestUserInputQuestion `json:"questions"`
}

type appServerRequestUserInputQuestion struct {
	ID       string                            `json:"id"`
	Header   string                            `json:"header"`
	Question string                            `json:"question"`
	IsOther  bool                              `json:"isOther"`
	IsSecret bool                              `json:"isSecret"`
	Options  []appServerRequestUserInputOption `json:"options"`
}

type appServerRequestUserInputOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type appServerRequestUserInputResponse struct {
	Answers map[string]appServerRequestUserInputAnswer `json:"answers"`
}

type appServerObservedRequestUserInput struct {
	rawID     json.RawMessage
	questions []appServerRequestUserInputQuestion
}

type appServerRequestUserInputAnswer struct {
	Answers []string `json:"answers"`
}

type appServerSession struct {
	transport        string
	url              string
	socketPath       string
	workDir          string
	model            string
	effort           string
	mode             string
	baseURL          string
	modelProvider    string
	extraEnv         []string
	codexHome        string
	promptPreamble   string
	observerProgress bool
	observerInput    bool

	events chan core.Event

	ctx    context.Context
	cancel context.CancelFunc

	cmd         *exec.Cmd
	wsConn      *websocket.Conn
	stdin       io.WriteCloser
	transportMu sync.Mutex
	writeMu     sync.Mutex

	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan rpcResponseEnvelope
	// pendingMethods lets response handling establish turn ownership before
	// the reader advances to a server request that immediately follows the
	// turn/start response on the same connection.
	pendingMethods map[int64]string

	approvalsMu      sync.Mutex
	pendingApprovals map[string]chan core.PermissionResult
	observedInputs   map[string]appServerObservedRequestUserInput
	permissionDone   chan string

	threadID atomic.Value
	alive    atomic.Bool

	bindingMu              sync.Mutex
	bufferNotifications    bool
	bufferNotificationsTo  time.Time
	replayingNotifications bool
	bufferedNotifications  []appServerBufferedNotification
	writerLeaseKey         string

	closeOnce sync.Once
	wg        sync.WaitGroup

	stateMu       sync.Mutex
	pendingMsgs   []string
	currentTurn   string
	ownedTurn     string
	completedTurn string
	preambleSent  bool

	runtimeMu sync.RWMutex
	usage     *core.UsageReport
	context   *core.ContextUsage
}

const (
	appServerTransportProcess      = "process"
	appServerTransportDaemon       = "daemon"
	appServerRequestTimeout        = 120 * time.Second
	appServerInitializeTimeout     = 15 * time.Second
	appServerUsageRefreshTimeout   = 1500 * time.Millisecond
	appServerMaxMessageSize        = 10 * 1024 * 1024
	appServerNotificationBufferTTL = 5 * time.Second
	appServerNotificationBufferMax = 64
	appServerPermissionResolved    = "__cc_connect_external_resolved__"
)

var appServerDaemonWriters = appServerDaemonWriterRegistry{
	owners: make(map[string]*appServerSession),
}

func newAppServerSession(ctx context.Context, transport, url, socketPath, workDir, model, effort, mode, resumeID, baseURL, modelProvider string, extraEnv []string, codexHome string, systemPrompt string, appendPrompt string) (*appServerSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &appServerSession{
		transport:        transport,
		url:              url,
		socketPath:       strings.TrimSpace(socketPath),
		workDir:          workDir,
		model:            model,
		effort:           effort,
		mode:             mode,
		baseURL:          baseURL,
		modelProvider:    modelProvider,
		extraEnv:         append([]string(nil), extraEnv...),
		codexHome:        strings.TrimSpace(codexHome),
		promptPreamble:   buildCodexPromptPreamble(systemPrompt, appendPrompt),
		events:           make(chan core.Event, 128),
		ctx:              sessionCtx,
		cancel:           cancel,
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
		observedInputs:   make(map[string]appServerObservedRequestUserInput),
		permissionDone:   make(chan string, 32),
		preambleSent:     resumeID != "" && resumeID != core.ContinueSession,
	}
	s.alive.Store(true)

	if err := s.connect(); err != nil {
		cancel()
		return nil, err
	}

	if err := s.initialize(); err != nil {
		_ = s.Close()
		if s.transport == appServerTransportDaemon {
			return nil, fmt.Errorf("%w (ensure the shared daemon is running and remote control is enabled with `codex app-server daemon enable-remote-control`)", err)
		}
		return nil, err
	}

	if err := s.ensureThread(resumeID); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.refreshUsage(context.Background()); err != nil {
		slog.Debug("codex app-server: initial rate limit fetch failed", "error", err)
	}

	return s, nil
}

func (s *appServerSession) connect() error {
	if s.transport == appServerTransportDaemon {
		return s.connectDaemon()
	}
	return s.connectProcess()
}

func (s *appServerSession) connectProcess() error {
	args := []string{"app-server"}
	if strings.TrimSpace(s.url) != "" {
		args = append(args, "--listen", strings.TrimSpace(s.url))
	}
	if model := strings.TrimSpace(s.model); model != "" {
		args = append(args, "-c", fmt.Sprintf("model=%q", model))
	}
	if effort := strings.TrimSpace(s.effort); effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort))
	}
	if provider := strings.TrimSpace(s.modelProvider); provider != "" {
		args = append(args, "-c", fmt.Sprintf("model_provider=%q", provider))
	}
	if baseURL := strings.TrimSpace(s.baseURL); baseURL != "" {
		args = append(args, "-c", fmt.Sprintf("openai_base_url=%q", baseURL))
	}
	cmd := exec.CommandContext(s.ctx, "codex", args...)
	cmd.Dir = s.workDir
	env := append([]string(nil), s.extraEnv...)
	if s.codexHome != "" {
		env = append(env, "CODEX_HOME="+s.codexHome)
	}
	if len(env) > 0 {
		cmd.Env = core.MergeEnv(os.Environ(), env)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("codex app-server stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("codex app-server stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("codex app-server stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codex app-server start: %w", err)
	}

	s.transportMu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.transportMu.Unlock()

	slog.Info("codex app-server session started", "transport", "process", "pid", cmd.Process.Pid, "work_dir", s.workDir)

	s.wg.Add(3)
	go s.readLoop(stdout)
	go s.stderrLoop(stderr)
	go s.waitLoop()
	return nil
}

func (s *appServerSession) connectDaemon() error {
	socketPath := strings.TrimSpace(s.socketPath)
	if socketPath == "" {
		return fmt.Errorf("codex app-server socket path is empty")
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: appServerInitializeTimeout,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var netDialer net.Dialer
			return netDialer.DialContext(ctx, "unix", socketPath)
		},
	}
	conn, response, err := dialer.DialContext(s.ctx, "ws://localhost/", nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
			return fmt.Errorf("codex app-server websocket handshake (%s): %w", response.Status, err)
		}
		return fmt.Errorf("codex app-server connect socket %q: %w", socketPath, err)
	}
	conn.SetReadLimit(appServerMaxMessageSize)

	s.transportMu.Lock()
	s.wsConn = conn
	s.transportMu.Unlock()

	slog.Info("codex app-server session connected", "transport", "websocket-unix", "socket", socketPath, "work_dir", s.workDir)

	s.wg.Add(1)
	go s.readWebSocketLoop(conn)
	return nil
}

func resolveAppServerSocket(explicitSocket, explicitCodexHome string, extraEnv []string) (string, error) {
	if socketPath := strings.TrimSpace(explicitSocket); socketPath != "" {
		return expandAppServerSocketPath(socketPath)
	}

	env := append([]string(nil), extraEnv...)
	if codexHome := strings.TrimSpace(explicitCodexHome); codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}
	codexHome, err := resolveCodexHome(env)
	if err != nil {
		return "", err
	}
	return expandAppServerSocketPath(filepath.Join(codexHome, "app-server-control", "app-server-control.sock"))
}

func expandAppServerSocketPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return filepath.Clean(path), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if path == "~" {
		return homeDir, nil
	}
	return filepath.Join(homeDir, strings.TrimPrefix(path, "~/")), nil
}

func (s *appServerSession) initialize() error {
	optOut := []string{
		"command/exec/outputDelta",
		"item/agentMessage/delta",
		"item/plan/delta",
		"item/fileChange/outputDelta",
		"item/reasoning/summaryTextDelta",
		"item/reasoning/textDelta",
	}
	if s.observerProgress {
		// Passive observers need only identity-bearing wakeups. The handler below
		// discards delta bodies and core re-reads sanitized Codex state.
		optOut = []string{
			"command/exec/outputDelta",
			"item/reasoning/summaryTextDelta",
			"item/reasoning/textDelta",
		}
	}
	params := map[string]any{
		"clientInfo": map[string]any{
			"name":    "cc-connect-codex-agent",
			"title":   "CC Connect Codex Agent",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"experimentalApi":           true,
			"optOutNotificationMethods": optOut,
		},
	}

	var resp initResponse
	var err error
	if s.transport == appServerTransportDaemon {
		err = s.requestWithTimeout("initialize", params, &resp, appServerInitializeTimeout)
	} else {
		err = s.request("initialize", params, &resp)
	}
	if err != nil {
		return fmt.Errorf("codex app-server initialize: %w", err)
	}
	if err := s.notify("initialized", nil); err != nil {
		return fmt.Errorf("codex app-server initialized notify: %w", err)
	}
	return nil
}

func (s *appServerSession) ensureThread(resumeID string) error {
	if resumeID != "" && resumeID != core.ContinueSession {
		resumeID = strings.TrimSpace(resumeID)
		if err := s.bindThread(resumeID); err != nil {
			return err
		}

		params := s.threadRequestParams()
		params["threadId"] = resumeID
		params["persistExtendedHistory"] = true

		var resp threadResumeResponse
		if err := s.request("thread/resume", params, &resp); err != nil {
			s.unbindThread()
			return err
		}
		if resp.Thread.ID == "" {
			s.unbindThread()
			return fmt.Errorf("codex app-server resume returned empty thread id")
		}
		if resp.Thread.ID != resumeID {
			s.unbindThread()
			return fmt.Errorf("codex app-server resume returned thread %q, want %q", resp.Thread.ID, resumeID)
		}
		s.applyThreadRuntimeState(resp.Cwd, resp.Model, resp.ReasoningEffort)
		slog.Info("codex app-server thread resumed", "thread_id", resp.Thread.ID)
		return nil
	}

	s.beginNotificationBuffer()
	var resp threadStartResponse
	if err := s.request("thread/start", s.threadStartRequestParams(), &resp); err != nil {
		s.discardNotificationBuffer()
		return err
	}
	if resp.Thread.ID == "" {
		s.discardNotificationBuffer()
		return fmt.Errorf("codex app-server start returned empty thread id")
	}
	s.applyThreadRuntimeState(resp.Cwd, resp.Model, resp.ReasoningEffort)
	if err := s.bindThread(resp.Thread.ID); err != nil {
		return err
	}
	slog.Info("codex app-server thread started", "thread_id", resp.Thread.ID)
	return nil
}

func (s *appServerSession) beginNotificationBuffer() {
	s.bindingMu.Lock()
	s.bufferNotifications = true
	s.bufferNotificationsTo = time.Now().Add(appServerNotificationBufferTTL)
	s.replayingNotifications = false
	s.bufferedNotifications = nil
	s.bindingMu.Unlock()
}

func (s *appServerSession) discardNotificationBuffer() {
	s.bindingMu.Lock()
	s.clearNotificationBufferLocked()
	s.bindingMu.Unlock()
}

func (s *appServerSession) clearNotificationBufferLocked() {
	s.bufferNotifications = false
	s.bufferNotificationsTo = time.Time{}
	s.replayingNotifications = false
	s.bufferedNotifications = nil
}

func (s *appServerSession) bindThread(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		s.discardNotificationBuffer()
		return fmt.Errorf("codex app-server thread id is empty")
	}

	s.bindingMu.Lock()
	current := s.CurrentSessionID()
	if current != "" && current != threadID {
		s.bindingMu.Unlock()
		return fmt.Errorf("codex app-server thread already bound to %q", current)
	}
	if s.transport == appServerTransportDaemon && s.writerLeaseKey == "" {
		if !s.alive.Load() {
			s.clearNotificationBufferLocked()
			s.bindingMu.Unlock()
			return fmt.Errorf("codex app-server connection closed before binding thread %q", threadID)
		}
		leaseKey := appServerDaemonWriterKey(s.socketPath, threadID)
		if err := appServerDaemonWriters.acquire(leaseKey, s); err != nil {
			s.clearNotificationBufferLocked()
			s.bindingMu.Unlock()
			return err
		}
		s.writerLeaseKey = leaseKey
		if !s.alive.Load() {
			appServerDaemonWriters.release(leaseKey, s)
			s.writerLeaseKey = ""
			s.clearNotificationBufferLocked()
			s.bindingMu.Unlock()
			return fmt.Errorf("codex app-server connection closed while binding thread %q", threadID)
		}
	}
	s.threadID.Store(threadID)

	now := time.Now()
	if !s.bufferNotificationsTo.IsZero() && now.After(s.bufferNotificationsTo) {
		s.bufferedNotifications = nil
	} else if len(s.bufferedNotifications) > 0 {
		matched := s.bufferedNotifications[:0]
		for _, notification := range s.bufferedNotifications {
			identity, ok := appServerMessageThreadIdentity(notification.params)
			if ok && identity.ThreadID == threadID {
				matched = append(matched, notification)
			}
		}
		s.bufferedNotifications = matched
	}
	s.bufferNotifications = false
	s.bufferNotificationsTo = time.Time{}
	s.replayingNotifications = len(s.bufferedNotifications) > 0
	s.bindingMu.Unlock()

	s.replayBufferedNotifications()
	return nil
}

// bindReadOnlyThread scopes daemon notifications to one validated thread
// without resuming the thread or acquiring the cc-connect writer lease. It is
// the base binding for passive observers and for interactive observers before
// their explicit thread/resume subscription.
func (s *appServerSession) bindReadOnlyThread(threadID string) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return fmt.Errorf("codex app-server observer thread id is empty")
	}
	if s.transport != appServerTransportDaemon {
		return fmt.Errorf("codex app-server read-only binding requires daemon transport")
	}

	s.bindingMu.Lock()
	defer s.bindingMu.Unlock()
	if !s.alive.Load() {
		return fmt.Errorf("codex app-server connection closed before observing thread %q", threadID)
	}
	if current := s.CurrentSessionID(); current != "" && current != threadID {
		return fmt.Errorf("codex app-server observer already bound to thread %q", current)
	}
	if s.writerLeaseKey != "" {
		return fmt.Errorf("codex app-server observer unexpectedly owns a writer lease")
	}
	s.threadID.Store(threadID)
	s.clearNotificationBufferLocked()
	return nil
}

func (s *appServerSession) unbindThread() {
	s.bindingMu.Lock()
	if s.writerLeaseKey != "" {
		appServerDaemonWriters.release(s.writerLeaseKey, s)
		s.writerLeaseKey = ""
	}
	s.threadID.Store("")
	s.clearNotificationBufferLocked()
	s.bindingMu.Unlock()
}

func (s *appServerSession) releaseWriterLease() {
	s.bindingMu.Lock()
	if s.writerLeaseKey != "" {
		appServerDaemonWriters.release(s.writerLeaseKey, s)
		s.writerLeaseKey = ""
	}
	s.bindingMu.Unlock()
}

func appServerDaemonWriterKey(socketPath, threadID string) string {
	endpoint := strings.TrimSpace(socketPath)
	if abs, err := filepath.Abs(endpoint); err == nil {
		endpoint = abs
	}
	if resolved, err := filepath.EvalSymlinks(endpoint); err == nil {
		endpoint = resolved
	}
	return filepath.Clean(endpoint) + "\x00" + strings.TrimSpace(threadID)
}

func (r *appServerDaemonWriterRegistry) acquire(key string, owner *appServerSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.owners[key]; current != nil && current != owner {
		return fmt.Errorf("%w: Codex thread %q", core.ErrAgentSessionWriterBusy, appServerWriterThreadID(key))
	}
	r.owners[key] = owner
	return nil
}

func (r *appServerDaemonWriterRegistry) release(key string, owner *appServerSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners[key] == owner {
		delete(r.owners, key)
	}
}

func (r *appServerDaemonWriterRegistry) owns(key string, owner *appServerSession) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return key != "" && r.owners[key] == owner
}

func appServerWriterThreadID(key string) string {
	if i := strings.LastIndexByte(key, 0); i >= 0 {
		return key[i+1:]
	}
	return ""
}

func (s *appServerSession) replayBufferedNotifications() {
	for {
		s.bindingMu.Lock()
		if len(s.bufferedNotifications) == 0 {
			s.replayingNotifications = false
			s.bindingMu.Unlock()
			return
		}
		buffered := append([]appServerBufferedNotification(nil), s.bufferedNotifications...)
		s.bufferedNotifications = nil
		s.bindingMu.Unlock()

		for _, notification := range buffered {
			s.dispatchNotification(notification.method, notification.params)
		}
	}
}

func (s *appServerSession) threadStartRequestParams() map[string]any {
	params := s.threadRequestParams()
	if s.transport != appServerTransportDaemon {
		return params
	}
	workDir := strings.TrimSpace(s.GetWorkDir())
	if workDir == "" {
		return params
	}
	if absWorkDir, err := filepath.Abs(workDir); err == nil {
		workDir = absWorkDir
	}
	params["cwd"] = workDir
	return params
}

func (s *appServerSession) threadRequestParams() map[string]any {
	params := map[string]any{
		"experimentalRawEvents":  false,
		"persistExtendedHistory": false,
	}
	if model := s.GetModel(); model != "" {
		params["model"] = model
	}
	if approval, sandbox := appServerModeSettings(s.mode); approval != "" {
		params["approvalPolicy"] = approval
		if sandbox != "" {
			params["sandbox"] = sandbox
		}
	}
	return params
}

func appServerModeSettings(mode string) (approval string, sandbox string) {
	switch normalizeMode(mode) {
	case "auto-edit", "full-auto":
		return "never", "workspace-write"
	case "yolo":
		return "never", "danger-full-access"
	default:
		return "on-request", "read-only"
	}
}

func (s *appServerSession) applyThreadRuntimeState(workDir, model string, effort *string) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if dir := strings.TrimSpace(workDir); dir != "" {
		s.workDir = dir
	}
	if m := strings.TrimSpace(model); m != "" {
		s.model = m
	}
	s.effort = normalizeRuntimeReasoningEffort(stringValue(effort))
}

func (s *appServerSession) refreshUsage(ctx context.Context) error {
	timeout := appServerUsageRefreshTimeout
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			if until := time.Until(deadline); until > 0 && until < timeout {
				timeout = until
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if timeout <= 0 {
		return context.DeadlineExceeded
	}

	var resp appServerRateLimitsResponse
	if err := s.requestWithTimeout("account/rateLimits/read", map[string]any{}, &resp, timeout); err != nil {
		return err
	}
	s.storeUsage(mapAppServerRateLimits(resp))
	return nil
}

func (s *appServerSession) cachedUsage() *core.UsageReport {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return cloneUsageReport(s.usage)
}

func (s *appServerSession) cachedContextUsage() *core.ContextUsage {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return cloneContextUsage(s.context)
}

func (s *appServerSession) storeUsage(report *core.UsageReport) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.usage = cloneUsageReport(report)
}

func (s *appServerSession) storeContextUsage(usage *core.ContextUsage) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.context = cloneContextUsage(usage)
}

func (s *appServerSession) Send(prompt string, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	return s.sendWithCollaborationMode(prompt, messageID, images, files, "")
}

func (s *appServerSession) SendWithCollaborationMode(prompt string, messageID string, images []core.ImageAttachment, files []core.FileAttachment, mode string) error {
	return s.sendWithCollaborationMode(prompt, messageID, images, files, mode)
}

func (s *appServerSession) sendWithCollaborationMode(prompt string, messageID string, images []core.ImageAttachment, files []core.FileAttachment, collaborationMode string) error {
	if !s.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	if len(files) > 0 {
		filePaths := core.SaveFilesToDisk(s.workDir, messageID, files)
		prompt = core.AppendFileRefs(prompt, filePaths)
	}

	prompt, imagePaths, err := s.stageImages(prompt, images)
	if err != nil {
		return err
	}

	s.stateMu.Lock()
	if !s.preambleSent {
		prompt = prependCodexPromptPreamble(prompt, s.promptPreamble)
		s.preambleSent = true
	}
	s.stateMu.Unlock()

	threadID := s.CurrentSessionID()
	if threadID == "" {
		return fmt.Errorf("codex app-server thread id is empty")
	}
	if !s.ownsWriterLease() {
		return fmt.Errorf("%w: Codex thread %q", core.ErrAgentSessionWriterBusy, threadID)
	}

	input := make([]map[string]any, 0, 1+len(imagePaths))
	input = append(input, map[string]any{
		"type":          "text",
		"text":          prompt,
		"text_elements": []any{},
	})
	for _, path := range imagePaths {
		input = append(input, map[string]any{
			"type": "localImage",
			"path": path,
		})
	}

	params := map[string]any{
		"threadId": threadID,
		"input":    input,
	}
	if clientID := strings.TrimSpace(messageID); clientID != "" {
		params["clientUserMessageId"] = clientID
	}
	if model := s.GetModel(); model != "" {
		params["model"] = model
	}
	if effort := s.GetReasoningEffort(); effort != "" {
		params["effort"] = effort
	}
	if approval, _ := appServerModeSettings(s.mode); approval != "" {
		params["approvalPolicy"] = approval
	}
	if collaborationMode = strings.ToLower(strings.TrimSpace(collaborationMode)); collaborationMode != "" {
		if collaborationMode != "default" && collaborationMode != "plan" {
			return fmt.Errorf("codex app-server: unsupported collaboration mode %q", collaborationMode)
		}
		model := s.GetModel()
		if model == "" {
			return fmt.Errorf("codex app-server: collaboration mode %q requires a model", collaborationMode)
		}
		settings := map[string]any{
			"model":                  model,
			"developer_instructions": nil,
		}
		if effort := s.GetReasoningEffort(); effort != "" {
			settings["reasoning_effort"] = effort
		}
		params["collaborationMode"] = map[string]any{
			"mode":     collaborationMode,
			"settings": settings,
		}
	}

	var resp turnStartResponse
	if err := s.request("turn/start", params, &resp); err != nil {
		return fmt.Errorf("codex app-server turn/start: %w", err)
	}
	if resp.Turn.ID == "" {
		return fmt.Errorf("codex app-server turn/start returned empty turn id")
	}

	return nil
}

func (s *appServerSession) stageImages(prompt string, images []core.ImageAttachment) (string, []string, error) {
	if len(images) == 0 {
		return prompt, nil, nil
	}

	imgDir := filepath.Join(s.workDir, ".cc-connect", "images")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("codex app-server: create image dir: %w", err)
	}

	imagePaths := make([]string, 0, len(images))
	for i, img := range images {
		ext := codexImageExt(img.MimeType)
		fname := fmt.Sprintf("img_%d_%d%s", time.Now().UnixMilli(), i, ext)
		fpath := filepath.Join(imgDir, fname)
		if err := os.WriteFile(fpath, img.Data, 0o644); err != nil {
			return "", nil, fmt.Errorf("codex app-server: save image: %w", err)
		}
		imagePaths = append(imagePaths, fpath)
	}

	if strings.TrimSpace(prompt) == "" {
		prompt = "Please analyze the attached image(s)."
	}

	return prompt, imagePaths, nil
}

func (s *appServerSession) RespondPermission(requestID string, result core.PermissionResult) error {
	s.approvalsMu.Lock()
	observed, observedOK := s.observedInputs[requestID]
	if observedOK {
		delete(s.observedInputs, requestID)
	}
	ch := s.pendingApprovals[requestID]
	if ch != nil {
		// Claim the request atomically against serverRequest/resolved. Without
		// removing it here, every locally answered prompt would also enqueue an
		// "external" resolution and eventually fill the side channel.
		delete(s.pendingApprovals, requestID)
	}
	s.approvalsMu.Unlock()
	if observedOK {
		response := appServerRequestUserInputResponseFromResult(observed.questions, result)
		if err := s.writeJSON(map[string]any{
			"jsonrpc": "2.0", "id": observed.rawID,
			"result": response,
		}); err != nil {
			return fmt.Errorf("codex app-server: answer observed user input request %s: %w", requestID, err)
		}
		return nil
	}
	if ch == nil {
		return fmt.Errorf("codex app-server: no pending approval for request %s", requestID)
	}
	select {
	case ch <- result:
	default:
	}
	return nil
}

func (s *appServerSession) PermissionResolutions() <-chan string {
	return s.permissionDone
}

func (s *appServerSession) handleServerRequest(probe map[string]json.RawMessage) {
	rawID := probe["id"]
	var method string
	if err := json.Unmarshal(probe["method"], &method); err != nil {
		return
	}
	params := probe["params"]
	if appServerThreadScopedServerRequest(method) {
		if !s.ownsServerRequest(method, params) {
			return
		}
	} else if s.transport == appServerTransportDaemon {
		// A shared connection must never answer an unclassified server request:
		// another daemon client may be its real owner. Known global requests are
		// intentionally left to a dedicated daemon controller.
		slog.Debug("codex app-server: ignoring unowned daemon server request", "method", method)
		return
	}

	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		s.handleApprovalRequest(rawID, method, params)
	case "item/permissions/requestApproval":
		s.handlePermissionsApproval(rawID, params)
	case "item/tool/requestUserInput":
		if s.observerInput {
			s.handleObservedRequestUserInput(rawID, params)
		} else {
			s.handleRequestUserInput(rawID, params)
		}
	case "item/tool/call":
		s.handleDynamicToolCall(rawID, params)
	default:
		_ = s.writeJSON(map[string]any{
			"jsonrpc": "2.0", "id": rawID,
			"error": map[string]any{"code": -32601, "message": "method not found"},
		})
	}
}

func appServerThreadScopedServerRequest(method string) bool {
	switch method {
	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval",
		"item/tool/requestUserInput",
		"item/tool/call",
		"mcpServer/elicitation/request":
		return true
	default:
		return false
	}
}

func appServerServerRequestRequiresTurn(method string) bool {
	// MCP elicitation allows a null turnId in the upstream schema, but this
	// adapter has no safe owner correlation for that case. Treat it like every
	// other passive server request and fail closed unless it matches the active
	// turn exactly.
	return appServerThreadScopedServerRequest(method)
}

func (s *appServerSession) ownsServerRequest(method string, paramsRaw json.RawMessage) bool {
	identity, ok := appServerMessageThreadIdentity(paramsRaw)
	if !ok {
		slog.Warn("codex app-server: dropping server request without valid thread identity", "method", method)
		return false
	}

	currentThread := s.CurrentSessionID()
	if currentThread == "" || identity.ThreadID != currentThread {
		slog.Debug("codex app-server: dropping server request for another thread",
			"method", method,
			"notification_thread", identity.ThreadID,
			"session_thread", currentThread,
		)
		return false
	}
	if s.observerInput && method == "item/tool/requestUserInput" {
		// thread/resume subscribes this exact observer connection to the
		// daemon's shared server-request stream. It intentionally owns no
		// turn writer lease, but may answer a blocking question for the bound
		// thread just like another TUI client.
		return identity.TurnID != ""
	}
	if !s.ownsWriterLease() {
		slog.Warn("codex app-server: dropping server request without writer lease", "method", method, "thread_id", currentThread)
		return false
	}
	if !appServerServerRequestRequiresTurn(method) {
		return true
	}

	s.stateMu.Lock()
	ownedTurn := s.ownedTurn
	s.stateMu.Unlock()
	if identity.TurnID == "" || identity.TurnID != ownedTurn {
		slog.Debug("codex app-server: dropping server request for inactive turn",
			"method", method,
			"request_turn", identity.TurnID,
			"owned_turn", ownedTurn,
			"thread_id", currentThread,
		)
		return false
	}
	return true
}

func appServerMessageThreadIdentity(paramsRaw json.RawMessage) (appServerThreadIdentity, bool) {
	var identity appServerThreadIdentity
	if len(paramsRaw) == 0 || json.Unmarshal(paramsRaw, &identity) != nil {
		return appServerThreadIdentity{}, false
	}
	identity.ThreadID = strings.TrimSpace(identity.ThreadID)
	identity.TurnID = strings.TrimSpace(identity.TurnID)
	return identity, identity.ThreadID != ""
}

func (s *appServerSession) handleApprovalRequest(rawID json.RawMessage, method string, paramsRaw json.RawMessage) {
	requestID := string(rawID)
	var params map[string]any
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return
	}

	toolName, toolInput := method, appServerJSON(params)
	switch method {
	case "item/commandExecution/requestApproval":
		toolName = "Bash"
		if cmd, _ := params["command"].(string); cmd != "" {
			toolInput = cmd
			if cwd, _ := params["cwd"].(string); cwd != "" {
				toolInput += "\n(in " + cwd + ")"
			}
		}
	case "item/fileChange/requestApproval":
		toolName = "Patch"
		if reason, _ := params["reason"].(string); reason != "" {
			toolInput = reason
		}
	}

	ch := make(chan core.PermissionResult, 1)
	s.approvalsMu.Lock()
	s.pendingApprovals[requestID] = ch
	s.approvalsMu.Unlock()

	s.flushPendingAsThinking()
	s.emit(core.Event{
		Type:         core.EventPermissionRequest,
		RequestID:    requestID,
		ToolName:     toolName,
		ToolInput:    toolInput,
		ToolInputRaw: params,
	})

	go func() {
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		var result core.PermissionResult
		select {
		case result = <-ch:
		case <-s.ctx.Done():
			result = core.PermissionResult{Behavior: "deny"}
		case <-timer.C:
			result = core.PermissionResult{Behavior: "deny"}
		}
		s.approvalsMu.Lock()
		delete(s.pendingApprovals, requestID)
		s.approvalsMu.Unlock()
		if result.Behavior == appServerPermissionResolved {
			return
		}

		decision := "decline"
		if strings.EqualFold(result.Behavior, "allow") {
			decision = "accept"
		}
		_ = s.writeJSON(map[string]any{
			"jsonrpc": "2.0", "id": rawID,
			"result": map[string]any{"decision": decision},
		})
	}()
}

func (s *appServerSession) handlePermissionsApproval(rawID json.RawMessage, paramsRaw json.RawMessage) {
	requestID := string(rawID)
	var params map[string]any
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return
	}

	ch := make(chan core.PermissionResult, 1)
	s.approvalsMu.Lock()
	s.pendingApprovals[requestID] = ch
	s.approvalsMu.Unlock()

	s.flushPendingAsThinking()
	s.emit(core.Event{
		Type:         core.EventPermissionRequest,
		RequestID:    requestID,
		ToolName:     "Permissions",
		ToolInput:    appServerJSON(params),
		ToolInputRaw: params,
	})

	go func() {
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		var result core.PermissionResult
		select {
		case result = <-ch:
		case <-s.ctx.Done():
			result = core.PermissionResult{Behavior: "deny"}
		case <-timer.C:
			result = core.PermissionResult{Behavior: "deny"}
		}
		s.approvalsMu.Lock()
		delete(s.pendingApprovals, requestID)
		s.approvalsMu.Unlock()
		if result.Behavior == appServerPermissionResolved {
			return
		}

		if strings.EqualFold(result.Behavior, "allow") {
			perms := params["permissions"]
			if perms == nil {
				perms = map[string]any{}
			}
			_ = s.writeJSON(map[string]any{
				"jsonrpc": "2.0", "id": rawID,
				"result": map[string]any{"permissions": perms, "scope": "turn"},
			})
		} else {
			_ = s.writeJSON(map[string]any{
				"jsonrpc": "2.0", "id": rawID,
				"result": map[string]any{"permissions": map[string]any{}},
			})
		}
	}()
}

func (s *appServerSession) handleRequestUserInput(rawID json.RawMessage, paramsRaw json.RawMessage) {
	requestID := string(rawID)
	var params appServerRequestUserInputParams
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		_ = s.writeJSON(map[string]any{
			"jsonrpc": "2.0", "id": rawID,
			"error": map[string]any{"code": -32602, "message": "invalid params"},
		})
		return
	}

	questions := appServerRequestUserInputQuestions(params.Questions)
	if len(questions) == 0 {
		_ = s.writeJSON(map[string]any{
			"jsonrpc": "2.0", "id": rawID,
			"result": appServerRequestUserInputResponse{Answers: map[string]appServerRequestUserInputAnswer{}},
		})
		return
	}

	rawInput := appServerRequestUserInputRawInput(params)
	ch := make(chan core.PermissionResult, 1)
	s.approvalsMu.Lock()
	s.pendingApprovals[requestID] = ch
	s.approvalsMu.Unlock()

	s.flushPendingAsThinking()
	s.emit(core.Event{
		Type:         core.EventPermissionRequest,
		ThreadID:     strings.TrimSpace(params.ThreadID),
		TurnID:       strings.TrimSpace(params.TurnID),
		ItemID:       strings.TrimSpace(params.ItemID),
		SessionID:    strings.TrimSpace(params.ThreadID),
		RequestID:    requestID,
		ToolName:     "AskUserQuestion",
		ToolInput:    appServerJSON(rawInput),
		ToolInputRaw: rawInput,
		Questions:    questions,
	})

	go func() {
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		var result core.PermissionResult
		select {
		case result = <-ch:
		case <-s.ctx.Done():
			result = core.PermissionResult{Behavior: "deny"}
		case <-timer.C:
			result = core.PermissionResult{Behavior: "deny"}
		}
		s.approvalsMu.Lock()
		delete(s.pendingApprovals, requestID)
		s.approvalsMu.Unlock()
		if result.Behavior == appServerPermissionResolved {
			return
		}

		response := appServerRequestUserInputResponseFromResult(params.Questions, result)
		_ = s.writeJSON(map[string]any{
			"jsonrpc": "2.0", "id": rawID,
			"result": response,
		})
	}()
}

func (s *appServerSession) handleObservedRequestUserInput(rawID json.RawMessage, paramsRaw json.RawMessage) {
	requestID := string(rawID)
	var params appServerRequestUserInputParams
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		slog.Warn("codex app-server: invalid observed request_user_input", "error", err)
		return
	}

	questions := appServerRequestUserInputQuestions(params.Questions)
	if len(questions) == 0 {
		return
	}

	s.approvalsMu.Lock()
	if s.observedInputs == nil {
		s.observedInputs = make(map[string]appServerObservedRequestUserInput)
	}
	s.observedInputs[requestID] = appServerObservedRequestUserInput{
		rawID:     append(json.RawMessage(nil), rawID...),
		questions: append([]appServerRequestUserInputQuestion(nil), params.Questions...),
	}
	s.approvalsMu.Unlock()
	turnID := strings.TrimSpace(params.TurnID)
	if turnID != "" {
		s.stateMu.Lock()
		if s.currentTurn == "" {
			s.currentTurn = turnID
		}
		s.stateMu.Unlock()
	}

	rawInput := appServerRequestUserInputRawInput(params)
	s.emit(core.Event{
		Type:         core.EventPermissionRequest,
		ThreadID:     strings.TrimSpace(params.ThreadID),
		TurnID:       strings.TrimSpace(params.TurnID),
		ItemID:       strings.TrimSpace(params.ItemID),
		SessionID:    strings.TrimSpace(params.ThreadID),
		RequestID:    requestID,
		ToolName:     "AskUserQuestion",
		ToolInput:    appServerJSON(rawInput),
		ToolInputRaw: rawInput,
		Questions:    questions,
	})
}

func (s *appServerSession) handleDynamicToolCall(rawID json.RawMessage, paramsRaw json.RawMessage) {
	_ = s.writeJSON(map[string]any{
		"jsonrpc": "2.0", "id": rawID,
		"result": map[string]any{
			"success":      false,
			"contentItems": []map[string]any{{"type": "inputText", "text": "tool not available on this client"}},
		},
	})
}

func appServerRequestUserInputQuestions(input []appServerRequestUserInputQuestion) []core.UserQuestion {
	questions := make([]core.UserQuestion, 0, len(input))
	for _, in := range input {
		questionText := strings.TrimSpace(in.Question)
		if questionText == "" {
			continue
		}
		q := core.UserQuestion{
			Question: questionText,
			Header:   strings.TrimSpace(in.Header),
			Secret:   in.IsSecret,
		}
		for _, opt := range in.Options {
			q.Options = append(q.Options, core.UserQuestionOption{
				Label:       strings.TrimSpace(opt.Label),
				Description: strings.TrimSpace(opt.Description),
			})
		}
		questions = append(questions, q)
	}
	return questions
}

func appServerRequestUserInputRawInput(params appServerRequestUserInputParams) map[string]any {
	questions := make([]any, 0, len(params.Questions))
	for _, in := range params.Questions {
		q := map[string]any{
			"id":       in.ID,
			"header":   in.Header,
			"question": in.Question,
			"isOther":  in.IsOther,
			"isSecret": in.IsSecret,
			"options":  appServerRequestUserInputRawOptions(in.Options),
		}
		questions = append(questions, q)
	}
	return map[string]any{
		"threadId":  params.ThreadID,
		"turnId":    params.TurnID,
		"itemId":    params.ItemID,
		"questions": questions,
	}
}

func appServerRequestUserInputRawOptions(options []appServerRequestUserInputOption) []any {
	out := make([]any, 0, len(options))
	for _, opt := range options {
		out = append(out, map[string]any{
			"label":       opt.Label,
			"description": opt.Description,
		})
	}
	return out
}

func appServerRequestUserInputResponseFromResult(questions []appServerRequestUserInputQuestion, result core.PermissionResult) appServerRequestUserInputResponse {
	response := appServerRequestUserInputResponse{Answers: map[string]appServerRequestUserInputAnswer{}}
	if !strings.EqualFold(result.Behavior, "allow") {
		return response
	}

	answersRaw, _ := result.UpdatedInput["answers"].(map[string]any)
	if len(answersRaw) == 0 {
		return response
	}

	for _, q := range questions {
		id := strings.TrimSpace(q.ID)
		text := strings.TrimSpace(q.Question)
		if id == "" || text == "" {
			continue
		}
		values := appServerRequestUserInputAnswerValues(answersRaw[text])
		if len(values) == 0 {
			continue
		}
		response.Answers[id] = appServerRequestUserInputAnswer{Answers: values}
	}
	return response
}

func appServerRequestUserInputAnswerValues(raw any) []string {
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []string:
		values := make([]string, 0, len(v))
		for _, s := range v {
			if strings.TrimSpace(s) != "" {
				values = append(values, s)
			}
		}
		return values
	case []any:
		values := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				values = append(values, s)
			}
		}
		return values
	case map[string]any:
		return appServerRequestUserInputAnswerValues(v["answers"])
	case appServerRequestUserInputAnswer:
		return appServerRequestUserInputAnswerValues(v.Answers)
	default:
		return nil
	}
}

func (s *appServerSession) rejectPendingApprovals(err error) {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	for id, ch := range s.pendingApprovals {
		delete(s.pendingApprovals, id)
		select {
		case ch <- core.PermissionResult{Behavior: "deny"}:
		default:
		}
	}
	for id := range s.observedInputs {
		delete(s.observedInputs, id)
	}
}

func (s *appServerSession) Events() <-chan core.Event {
	return s.events
}

func (s *appServerSession) CurrentSessionID() string {
	v, _ := s.threadID.Load().(string)
	return v
}

func (s *appServerSession) GetWorkDir() string {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.workDir
}

func (s *appServerSession) GetModel() string {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return strings.TrimSpace(s.model)
}

func (s *appServerSession) GetReasoningEffort() string {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return strings.TrimSpace(s.effort)
}

func (s *appServerSession) GetUsage(ctx context.Context) (*core.UsageReport, error) {
	if err := s.refreshUsage(ctx); err != nil {
		if cached := s.cachedUsage(); cached != nil {
			return cached, nil
		}
		return nil, err
	}
	if cached := s.cachedUsage(); cached != nil {
		return cached, nil
	}
	return nil, fmt.Errorf("codex app-server usage unavailable")
}

func (s *appServerSession) GetContextUsage() *core.ContextUsage {
	return s.cachedContextUsage()
}

func (s *appServerSession) Alive() bool {
	return s.alive.Load()
}

// RelayUnsolicitedEvents keeps turns started by another client on a shared
// daemon connection silent. Foreground cc-connect turns still use the normal
// event reader and are delivered unchanged.
func (s *appServerSession) RelayUnsolicitedEvents() bool {
	return s.transport != appServerTransportDaemon
}

func (s *appServerSession) Close() error {
	s.alive.Store(false)
	if s.cancel != nil {
		s.cancel()
	}

	// Daemon transport owns only the client connection; process transport owns
	// the private child process and tears it down with the session.
	s.transportMu.Lock()
	conn := s.wsConn
	s.wsConn = nil
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.transportMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	s.releaseWriterLease()

	s.closeOnce.Do(func() {
		close(s.events)
	})
	return nil
}

func (s *appServerSession) ownsWriterLease() bool {
	if s.transport != appServerTransportDaemon {
		return true
	}
	s.bindingMu.Lock()
	key := s.writerLeaseKey
	s.bindingMu.Unlock()
	return appServerDaemonWriters.owns(key, s)
}

func (s *appServerSession) readLoop(r io.Reader) {
	defer s.wg.Done()
	scanner := bufio.NewScanner(r)
	scanBuf := make([]byte, 0, 64*1024)
	const maxLineSize = 10 * 1024 * 1024
	scanner.Buffer(scanBuf, maxLineSize)

	for scanner.Scan() {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		s.handleIncomingMessage(scanner.Bytes())
	}

	err := scanner.Err()
	if err != nil {
		if s.ctx.Err() == nil && !errors.Is(err, io.EOF) {
			slog.Warn("codex app-server read failed", "error", err)
			if errors.Is(err, bufio.ErrTooLong) {
				s.emitError(fmt.Errorf("codex app-server line exceeds max size (%d bytes): %w", maxLineSize, err))
			} else {
				s.emitError(fmt.Errorf("codex app-server connection closed: %w", err))
			}
		}
		s.alive.Store(false)
		s.rejectPending(err)
		s.rejectPendingApprovals(err)
		return
	}

	s.alive.Store(false)
	s.rejectPending(io.EOF)
	s.rejectPendingApprovals(io.EOF)
}

func (s *appServerSession) stderrLoop(r io.Reader) {
	defer s.wg.Done()
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		slog.Debug("codex app-server stderr", "line", line)
	}
	if err := scanner.Err(); err != nil && s.ctx.Err() == nil {
		slog.Debug("codex app-server stderr read failed", "error", err)
	}
}

func (s *appServerSession) waitLoop() {
	defer s.wg.Done()

	s.transportMu.Lock()
	cmd := s.cmd
	s.transportMu.Unlock()
	if cmd == nil {
		return
	}

	err := cmd.Wait()
	if s.ctx.Err() == nil && err != nil {
		slog.Warn("codex app-server exited unexpectedly", "error", err)
		s.emitError(fmt.Errorf("codex app-server exited: %w", err))
	}
	s.alive.Store(false)
	if err == nil {
		err = io.EOF
	}
	s.rejectPending(err)
}

func (s *appServerSession) readWebSocketLoop(conn *websocket.Conn) {
	defer func() {
		// A disconnected daemon client can no longer own the write side. Release
		// its lease here as well as in Close so a replacement connection can
		// resume the same thread without waiting for Engine cleanup.
		s.releaseWriterLease()
		s.alive.Store(false)
		s.wg.Done()
	}()
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			if s.ctx.Err() == nil {
				// Publish the dead state before EventError so Core cannot observe the
				// failure and immediately retry while the stale lease is still held.
				s.releaseWriterLease()
				s.alive.Store(false)
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					slog.Warn("codex app-server websocket read failed", "error", err)
				}
				// Even a peer-initiated normal close ends this AgentSession. Surface
				// it so a foreground/unsolicited reader does not wait forever on an
				// event channel that intentionally stays open until Close.
				s.emitError(fmt.Errorf("codex app-server connection closed: %w", err))
				s.rejectPending(err)
				s.rejectPendingApprovals(err)
			}
			return
		}
		if messageType != websocket.TextMessage {
			slog.Debug("codex app-server: ignoring non-text websocket frame", "message_type", messageType)
			continue
		}
		s.handleIncomingMessage(data)
	}
}

func (s *appServerSession) handleIncomingMessage(data []byte) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		slog.Debug("codex app-server: invalid JSON", "error", err)
		return
	}

	_, hasID := probe["id"]
	_, hasMethod := probe["method"]

	switch {
	case hasID && !hasMethod:
		var resp rpcResponseEnvelope
		if err := json.Unmarshal(data, &resp); err != nil {
			slog.Debug("codex app-server: bad response envelope", "error", err)
			return
		}
		s.handleResponse(resp)
	case hasID && hasMethod:
		s.handleServerRequest(probe)
	default:
		var notif rpcNotificationEnvelope
		if err := json.Unmarshal(data, &notif); err != nil {
			slog.Debug("codex app-server: bad notification envelope", "error", err)
			return
		}
		s.handleNotification(notif.Method, notif.Params)
	}
}

func (s *appServerSession) handleResponse(resp rpcResponseEnvelope) {
	id, ok := rpcIDToInt64(resp.ID)
	if !ok {
		return
	}

	s.pendingMu.Lock()
	ch := s.pending[id]
	method := s.pendingMethods[id]
	delete(s.pending, id)
	delete(s.pendingMethods, id)
	s.pendingMu.Unlock()

	if ch == nil {
		return
	}
	if method == "turn/start" && resp.Error == nil {
		var turnResp turnStartResponse
		if json.Unmarshal(resp.Result, &turnResp) == nil && turnResp.Turn.ID != "" {
			s.recordOwnedTurn(turnResp.Turn.ID)
		}
	}

	select {
	case ch <- resp:
	default:
	}
}

func (s *appServerSession) handleNotification(method string, paramsRaw json.RawMessage) {
	switch classifyAppServerNotification(method) {
	case appServerNotificationThread:
		identity, ok := appServerMessageThreadIdentity(paramsRaw)
		if !ok {
			slog.Warn("codex app-server: dropping notification without valid thread identity", "method", method)
			return
		}
		if !s.acceptThreadNotification(method, paramsRaw, identity.ThreadID) {
			return
		}
	case appServerNotificationGlobal:
		// Explicitly global notifications are safe to process on every daemon
		// connection. All other handled notifications must pass the thread gate.
	default:
		return
	}

	s.dispatchNotification(method, paramsRaw)
}

type appServerNotificationScope uint8

const (
	appServerNotificationIgnored appServerNotificationScope = iota
	appServerNotificationThread
	appServerNotificationGlobal
)

func classifyAppServerNotification(method string) appServerNotificationScope {
	switch method {
	case "turn/started",
		"item/started",
		"item/completed",
		"item/agentMessage/delta",
		"item/plan/delta",
		"item/commandExecution/outputDelta",
		"item/fileChange/outputDelta",
		"turn/plan/updated",
		"turn/completed",
		"thread/status/changed",
		"thread/tokenUsage/updated",
		"serverRequest/resolved",
		"error":
		return appServerNotificationThread
	case "account/rateLimits/updated":
		return appServerNotificationGlobal
	default:
		return appServerNotificationIgnored
	}
}

func (s *appServerSession) acceptThreadNotification(method string, paramsRaw json.RawMessage, threadID string) bool {
	s.bindingMu.Lock()
	currentThread := s.CurrentSessionID()
	if currentThread != "" {
		if threadID != currentThread {
			s.bindingMu.Unlock()
			slog.Debug("codex app-server: dropping notification for another thread",
				"method", method,
				"notification_thread", threadID,
				"session_thread", currentThread,
			)
			return false
		}
		if s.replayingNotifications {
			s.bufferNotificationLocked(method, paramsRaw)
			s.bindingMu.Unlock()
			return false
		}
		s.bindingMu.Unlock()
		return true
	}

	if s.bufferNotifications && time.Now().Before(s.bufferNotificationsTo) {
		s.bufferNotificationLocked(method, paramsRaw)
		s.bindingMu.Unlock()
		return false
	}
	if s.bufferNotifications {
		s.clearNotificationBufferLocked()
	}
	s.bindingMu.Unlock()
	slog.Debug("codex app-server: dropping notification before thread binding", "method", method, "notification_thread", threadID)
	return false
}

func (s *appServerSession) bufferNotificationLocked(method string, paramsRaw json.RawMessage) {
	if len(s.bufferedNotifications) >= appServerNotificationBufferMax {
		slog.Warn("codex app-server: notification buffer full, dropping notification",
			"method", method,
			"max_notifications", appServerNotificationBufferMax,
		)
		return
	}
	s.bufferedNotifications = append(s.bufferedNotifications, appServerBufferedNotification{
		method: method,
		params: append(json.RawMessage(nil), paramsRaw...),
	})
}

func (s *appServerSession) dispatchNotification(method string, paramsRaw json.RawMessage) {
	switch method {
	case "turn/started":
		var notif turnNotification
		if err := json.Unmarshal(paramsRaw, &notif); err == nil {
			accepted := false
			s.stateMu.Lock()
			if notif.Turn.ID != "" && notif.Turn.ID != s.completedTurn {
				s.currentTurn = notif.Turn.ID
				s.pendingMsgs = s.pendingMsgs[:0]
				accepted = true
			}
			s.stateMu.Unlock()
			if accepted {
				s.storeContextUsage(nil)
				s.emit(core.Event{
					Type:      core.EventTurnStarted,
					ThreadID:  strings.TrimSpace(notif.ThreadID),
					TurnID:    strings.TrimSpace(notif.Turn.ID),
					SessionID: strings.TrimSpace(notif.ThreadID),
				})
			}
		}

	case "item/agentMessage/delta", "item/plan/delta", "item/commandExecution/outputDelta", "item/fileChange/outputDelta", "turn/plan/updated":
		// Foreground consumers render logical item events and do not consume
		// snapshot wakeups. Emitting every token/output delta here can fill the
		// event channel and hide a later tool result or terminal event.
		if !s.observerProgress {
			return
		}
		var notif struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
		}
		if err := json.Unmarshal(paramsRaw, &notif); err == nil {
			s.emit(core.Event{
				Type: core.EventConversationChanged, ThreadID: strings.TrimSpace(notif.ThreadID),
				TurnID: strings.TrimSpace(notif.TurnID), ItemID: strings.TrimSpace(notif.ItemID),
				SessionID: strings.TrimSpace(notif.ThreadID),
			})
		}

	case "item/started":
		var notif itemNotification
		if err := json.Unmarshal(paramsRaw, &notif); err == nil {
			s.handleItemStarted(notif)
		}

	case "item/completed":
		var notif itemNotification
		if err := json.Unmarshal(paramsRaw, &notif); err == nil {
			s.handleItemCompleted(notif)
		}

	case "turn/completed":
		var notif turnNotification
		if err := json.Unmarshal(paramsRaw, &notif); err == nil {
			s.completeTurn(notif.ThreadID, notif.Turn.ID, notif.Turn.Status)
		}

	case "thread/status/changed":
		var notif struct {
			ThreadID string `json:"threadId"`
			Status   struct {
				Type string `json:"type"`
			} `json:"status"`
		}
		if err := json.Unmarshal(paramsRaw, &notif); err == nil && notif.Status.Type == "idle" {
			// In codex 0.125+, thread going idle signals turn completion.
			s.completeTurn(notif.ThreadID, "", "completed")
		}

	case "serverRequest/resolved":
		var notif struct {
			ThreadID  string          `json:"threadId"`
			RequestID json.RawMessage `json:"requestId"`
		}
		if err := json.Unmarshal(paramsRaw, &notif); err == nil && len(notif.RequestID) > 0 {
			requestID := string(notif.RequestID)
			s.resolveServerRequest(strings.TrimSpace(notif.ThreadID), requestID)
		}

	case "account/rateLimits/updated":
		var notif appServerRateLimitsResponse
		if err := json.Unmarshal(paramsRaw, &notif); err == nil {
			s.storeUsage(mapAppServerRateLimits(notif))
		}

	case "thread/tokenUsage/updated":
		var notif appServerThreadTokenUsageNotification
		if err := json.Unmarshal(paramsRaw, &notif); err == nil {
			s.storeContextUsage(mapAppServerTokenUsage(notif))
		}

	case "error":
		var notif errorNotification
		if err := json.Unmarshal(paramsRaw, &notif); err == nil {
			message := strings.TrimSpace(notif.Error.Message)
			if message == "" {
				message = strings.TrimSpace(notif.Message)
			}
			if message != "" {
				s.emit(core.Event{
					Type:      core.EventError,
					ThreadID:  strings.TrimSpace(notif.ThreadID),
					TurnID:    strings.TrimSpace(notif.TurnID),
					SessionID: strings.TrimSpace(notif.ThreadID),
					Error:     fmt.Errorf("%s", message),
				})
			}
		}
	}
}

func (s *appServerSession) resolveServerRequest(threadID, requestID string) {
	s.approvalsMu.Lock()
	ch := s.pendingApprovals[requestID]
	delete(s.pendingApprovals, requestID)
	delete(s.observedInputs, requestID)
	s.approvalsMu.Unlock()
	if ch != nil {
		select {
		case ch <- core.PermissionResult{Behavior: appServerPermissionResolved}:
		default:
		}
	}
	// Only a request still owned by the foreground AgentSession needs the side
	// channel that unblocks core's permission wait. Observer requests use the
	// regular EventPermissionResolved event below.
	if ch != nil && s.permissionDone != nil {
		select {
		case s.permissionDone <- requestID:
		default:
			slog.Warn("codex app-server: permission resolution channel full", "request_id", requestID)
		}
	}
	s.emit(core.Event{
		Type:      core.EventPermissionResolved,
		ThreadID:  threadID,
		SessionID: threadID,
		RequestID: requestID,
	})
}

func (s *appServerSession) handleItemStarted(notif itemNotification) {
	item := notif.Item
	itemType, _ := item["type"].(string)
	if itemType == "" {
		return
	}

	switch itemType {
	case "agentMessage", "reasoning", "userMessage", "plan", "hookPrompt", "contextCompaction":
		return
	}
	if appServerConversationRequestUserInputItem(itemType, item) {
		// request_user_input has a dedicated EventPermissionRequest and card.
		// Flush preceding commentary, but never expose its transport arguments
		// as an ordinary tool invocation in the live progress card.
		s.flushPendingAsThinking()
		return
	}

	s.flushPendingAsThinking()
	if event, ok := appServerToolUseEvent(item); ok {
		s.emit(appServerItemEvent(notif, event))
	}
}

func (s *appServerSession) handleItemCompleted(notif itemNotification) {
	item := notif.Item
	itemType, _ := item["type"].(string)
	if itemType == "" {
		return
	}
	if appServerConversationRequestUserInputItem(itemType, item) {
		// The corresponding result is protocol JSON, not user-facing tool
		// output. The permission request lifecycle renders the actual answer.
		return
	}

	switch itemType {
	case "reasoning":
		text := appServerReasoningText(item)
		if text != "" {
			s.emit(appServerItemEvent(notif, core.Event{Type: core.EventThinking, Content: text}))
		}

	case "agentMessage", "plan":
		text, _ := item["text"].(string)
		if strings.TrimSpace(text) != "" {
			s.stateMu.Lock()
			s.pendingMsgs = append(s.pendingMsgs, text)
			s.stateMu.Unlock()
		}

	default:
		if event, ok := appServerToolResultEvent(item); ok {
			s.emit(appServerItemEvent(notif, event))
		}
	}
}

func appServerToolUseEvent(item map[string]any) (core.Event, bool) {
	switch itemType, _ := item["type"].(string); itemType {
	case "commandExecution":
		command, _ := item["command"].(string)
		return core.Event{Type: core.EventToolUse, ToolName: "Bash", ToolInput: command}, true
	case "mcpToolCall":
		server, _ := item["server"].(string)
		tool, _ := item["tool"].(string)
		name := strings.Trim(strings.Join([]string{server, tool}, ":"), ":")
		return core.Event{Type: core.EventToolUse, ToolName: "MCP", ToolInput: name + "\n" + appServerJSON(item["arguments"])}, true
	case "webSearch":
		query, _ := item["query"].(string)
		return core.Event{Type: core.EventToolUse, ToolName: "WebSearch", ToolInput: query}, true
	case "dynamicToolCall":
		tool, _ := item["tool"].(string)
		return core.Event{Type: core.EventToolUse, ToolName: tool, ToolInput: appServerJSON(item["arguments"])}, true
	case "fileChange":
		return core.Event{Type: core.EventToolUse, ToolName: "Patch", ToolInput: appServerJSON(item["changes"])}, true
	default:
		return core.Event{}, false
	}
}

func appServerToolResultEvent(item map[string]any) (core.Event, bool) {
	switch itemType, _ := item["type"].(string); itemType {
	case "commandExecution":
		command, _ := item["command"].(string)
		exitCode, hasExitCode := toInt(item["exitCode"])
		var exitCodePtr *int
		if hasExitCode {
			exitCodePtr = &exitCode
		}
		status := stringMapValue(item, "status")
		success := appServerToolSuccess(status, exitCodePtr)
		return core.Event{
			Type: core.EventToolResult, ToolName: "Bash", ToolInput: command,
			ToolResult: truncate(stringMapValue(item, "aggregatedOutput"), 500),
			ToolStatus: status, ToolExitCode: exitCodePtr, ToolSuccess: &success,
		}, true
	case "mcpToolCall":
		status := stringMapValue(item, "status")
		result := appServerJSON(item["result"])
		if errText := appServerJSON(item["error"]); strings.TrimSpace(errText) != "" && result == "" {
			result = errText
		}
		success := appServerToolSuccess(status, nil)
		return core.Event{
			Type: core.EventToolResult, ToolName: stringMapValue(item, "tool"),
			ToolResult: truncate(strings.TrimSpace(result), 500), ToolStatus: status, ToolSuccess: &success,
		}, true
	case "webSearch":
		return core.Event{
			Type: core.EventToolResult, ToolName: "WebSearch",
			ToolResult: truncate(stringMapValue(item, "query"), 500),
		}, true
	case "dynamicToolCall":
		status := stringMapValue(item, "status")
		success := appServerToolSuccess(status, nil)
		return core.Event{
			Type: core.EventToolResult, ToolName: stringMapValue(item, "tool"),
			ToolResult: truncate(strings.TrimSpace(appServerDynamicToolText(item["contentItems"])), 500),
			ToolStatus: status, ToolSuccess: &success,
		}, true
	default:
		return core.Event{}, false
	}
}

func appServerItemEvent(notif itemNotification, event core.Event) core.Event {
	event.ThreadID = strings.TrimSpace(notif.ThreadID)
	event.TurnID = strings.TrimSpace(notif.TurnID)
	event.SessionID = event.ThreadID
	event.ItemID = stringMapValue(notif.Item, "id")
	if event.ClientUserMessageID == "" {
		event.ClientUserMessageID = stringMapValue(notif.Item, "clientId")
	}
	return event
}

func appServerReasoningText(item map[string]any) string {
	var parts []string
	if summary, ok := item["summary"].([]any); ok {
		for _, entry := range summary {
			if text, ok := entry.(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 0 {
		if content, ok := item["content"].([]any); ok {
			for _, entry := range content {
				if text, ok := entry.(string); ok && strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

func appServerDynamicToolText(raw any) string {
	items, ok := raw.([]any)
	if !ok {
		return appServerJSON(raw)
	}
	var parts []string
	for _, entry := range items {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if text, _ := m["text"].(string); strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return appServerJSON(raw)
	}
	return strings.Join(parts, "\n")
}

func appServerToolSuccess(status string, exitCode *int) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	if exitCode != nil {
		return *exitCode == 0
	}
	return s == "completed" || s == "success" || s == "succeeded" || s == "ok"
}

func mapAppServerRateLimits(payload appServerRateLimitsResponse) *core.UsageReport {
	report := &core.UsageReport{Provider: "codex"}

	var snapshots []appServerRateLimitSnapshot
	if len(payload.RateLimitsByLimitID) > 0 {
		keys := make([]string, 0, len(payload.RateLimitsByLimitID))
		for key := range payload.RateLimitsByLimitID {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			snapshots = append(snapshots, payload.RateLimitsByLimitID[key])
		}
	} else if payload.RateLimits.LimitID != "" || payload.RateLimits.Primary != nil || payload.RateLimits.Secondary != nil || payload.RateLimits.Credits != nil {
		snapshots = append(snapshots, payload.RateLimits)
	}

	for _, snapshot := range snapshots {
		if report.Plan == "" && strings.TrimSpace(snapshot.PlanType) != "" {
			report.Plan = strings.TrimSpace(snapshot.PlanType)
		}
		if report.Credits == nil && snapshot.Credits != nil {
			report.Credits = &core.UsageCredits{
				HasCredits: snapshot.Credits.HasCredits,
				Unlimited:  snapshot.Credits.Unlimited,
			}
			if snapshot.Credits.Balance != nil {
				report.Credits.Balance = strings.TrimSpace(*snapshot.Credits.Balance)
			}
		}

		windows := appServerUsageWindows(snapshot)
		if len(windows) == 0 {
			continue
		}
		limitReached := false
		for _, window := range windows {
			if window.UsedPercent >= 100 {
				limitReached = true
				break
			}
		}

		report.Buckets = append(report.Buckets, core.UsageBucket{
			Name:         appServerBucketName(snapshot),
			Allowed:      !limitReached,
			LimitReached: limitReached,
			Windows:      windows,
		})
	}

	return report
}

func appServerBucketName(snapshot appServerRateLimitSnapshot) string {
	if name := strings.TrimSpace(snapshot.LimitName); name != "" {
		return name
	}
	if id := strings.TrimSpace(snapshot.LimitID); id != "" {
		return id
	}
	return "Rate limit"
}

func appServerUsageWindows(snapshot appServerRateLimitSnapshot) []core.UsageWindow {
	var windows []core.UsageWindow
	if snapshot.Primary != nil {
		windows = append(windows, appServerUsageWindow("Primary", snapshot.Primary))
	}
	if snapshot.Secondary != nil {
		windows = append(windows, appServerUsageWindow("Secondary", snapshot.Secondary))
	}
	return windows
}

func appServerUsageWindow(name string, window *appServerRateLimitWindow) core.UsageWindow {
	resetAfter := 0
	if window != nil && window.ResetsAt > 0 {
		resetAfter = int(time.Until(time.Unix(window.ResetsAt, 0)).Seconds())
		if resetAfter < 0 {
			resetAfter = 0
		}
	}
	return core.UsageWindow{
		Name:              name,
		UsedPercent:       window.UsedPercent,
		WindowSeconds:     window.WindowDurationMins * 60,
		ResetAfterSeconds: resetAfter,
		ResetAtUnix:       window.ResetsAt,
	}
}

func cloneUsageReport(report *core.UsageReport) *core.UsageReport {
	if report == nil {
		return nil
	}
	cloned := *report
	if len(report.Buckets) > 0 {
		cloned.Buckets = make([]core.UsageBucket, len(report.Buckets))
		for i, bucket := range report.Buckets {
			cloned.Buckets[i] = bucket
			if len(bucket.Windows) > 0 {
				cloned.Buckets[i].Windows = append([]core.UsageWindow(nil), bucket.Windows...)
			}
		}
	}
	if report.Credits != nil {
		credits := *report.Credits
		cloned.Credits = &credits
	}
	return &cloned
}

func normalizeRuntimeReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return ""
	case "med":
		return "medium"
	case "x-high", "very-high":
		return "xhigh"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

func appServerJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "{}" || s == "[]" || s == `""` {
		return ""
	}
	return s
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i), true
		}
	}
	return 0, false
}

func rpcIDToInt64(v any) (int64, bool) {
	switch id := v.(type) {
	case float64:
		return int64(id), true
	case int64:
		return id, true
	case int:
		return int64(id), true
	case json.Number:
		i, err := id.Int64()
		return i, err == nil
	}
	return 0, false
}

func (s *appServerSession) recordOwnedTurn(turnID string) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.completedTurn == turnID {
		return
	}
	s.currentTurn = turnID
	s.ownedTurn = turnID
	s.pendingMsgs = s.pendingMsgs[:0]
}

func (s *appServerSession) completeTurn(threadID, turnID, status string) {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	s.stateMu.Lock()
	if s.currentTurn == "" {
		if turnID != "" {
			s.completedTurn = turnID
		}
		s.stateMu.Unlock()
		return
	}
	if turnID != "" && turnID != s.currentTurn {
		s.completedTurn = turnID
		s.stateMu.Unlock()
		return
	}
	completedTurn := s.currentTurn
	s.currentTurn = ""
	if s.ownedTurn == completedTurn {
		s.ownedTurn = ""
	}
	s.completedTurn = completedTurn
	s.stateMu.Unlock()
	if threadID == "" {
		threadID = s.CurrentSessionID()
	}
	s.flushPendingAsText(threadID, completedTurn)
	s.emit(core.Event{
		Type:      core.EventResult,
		ThreadID:  threadID,
		TurnID:    completedTurn,
		SessionID: threadID,
		Done:      true,
		Metadata:  map[string]any{"turn_status": strings.TrimSpace(status)},
	})
}

func (s *appServerSession) flushPendingAsThinking() {
	s.stateMu.Lock()
	msgs := append([]string(nil), s.pendingMsgs...)
	s.pendingMsgs = s.pendingMsgs[:0]
	turnID := s.currentTurn
	s.stateMu.Unlock()
	threadID := s.CurrentSessionID()

	for _, text := range msgs {
		if strings.TrimSpace(text) != "" {
			s.emit(core.Event{
				Type:      core.EventThinking,
				ThreadID:  threadID,
				TurnID:    turnID,
				SessionID: threadID,
				Content:   text,
			})
		}
	}
}

func (s *appServerSession) flushPendingAsText(threadID, turnID string) {
	s.stateMu.Lock()
	msgs := append([]string(nil), s.pendingMsgs...)
	s.pendingMsgs = s.pendingMsgs[:0]
	s.stateMu.Unlock()
	if threadID == "" {
		threadID = s.CurrentSessionID()
	}

	for _, text := range msgs {
		if strings.TrimSpace(text) != "" {
			s.emit(core.Event{
				Type:      core.EventText,
				ThreadID:  threadID,
				TurnID:    turnID,
				SessionID: threadID,
				Content:   text,
			})
		}
	}
}

func (s *appServerSession) emit(event core.Event) {
	if appServerEventRequiresDelivery(event) {
		select {
		case s.events <- event:
		case <-s.ctx.Done():
		}
		return
	}
	select {
	case s.events <- event:
	default:
		// Conversation-changed events are edge-triggered snapshot wakeups. One
		// queued wakeup is sufficient; all logical events use backpressure above.
	}
}

func appServerEventRequiresDelivery(event core.Event) bool {
	return event.Type != core.EventConversationChanged
}

func appServerObserverEventRequiresDelivery(event core.Event) bool {
	switch event.Type {
	case core.EventPermissionRequest, core.EventPermissionResolved, core.EventResult, core.EventError:
		return true
	default:
		return false
	}
}

func (s *appServerSession) emitError(err error) {
	if err == nil {
		return
	}
	s.emit(core.Event{Type: core.EventError, Error: err})
}

func (s *appServerSession) rejectPending(err error) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for id, ch := range s.pending {
		delete(s.pending, id)
		delete(s.pendingMethods, id)
		select {
		case ch <- rpcResponseEnvelope{ID: id, Error: &rpcError{Message: err.Error()}}:
		default:
		}
	}
}

func (s *appServerSession) request(method string, params any, out any) error {
	return s.requestWithTimeout(method, params, out, appServerRequestTimeout)
}

func (s *appServerSession) requestWithTimeout(method string, params any, out any, timeout time.Duration) error {
	id := s.nextID.Add(1)
	ch := make(chan rpcResponseEnvelope, 1)

	s.pendingMu.Lock()
	if s.pending == nil {
		s.pending = make(map[int64]chan rpcResponseEnvelope)
	}
	if s.pendingMethods == nil {
		s.pendingMethods = make(map[int64]string)
	}
	s.pending[id] = ch
	s.pendingMethods[id] = method
	s.pendingMu.Unlock()

	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}

	deadline := time.Now().Add(timeout)
	if err := s.writeJSONWithTimeout(method, payload, timeout); err != nil {
		s.removePendingRequest(id)
		return err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		s.removePendingRequest(id)
		return fmt.Errorf("%s timed out", method)
	}

	timer := time.NewTimer(remaining)
	defer timer.Stop()
	ctxDone := s.contextDone()
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("%s", strings.TrimSpace(resp.Error.Message))
		}
		if out != nil {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("decode %s response: %w", method, err)
			}
		}
		return nil
	case <-ctxDone:
		s.removePendingRequest(id)
		return s.contextErr()
	case <-timer.C:
		s.removePendingRequest(id)
		return fmt.Errorf("%s timed out", method)
	}
}

func (s *appServerSession) removePendingRequest(id int64) {
	s.pendingMu.Lock()
	delete(s.pending, id)
	delete(s.pendingMethods, id)
	s.pendingMu.Unlock()
}

func (s *appServerSession) writeJSONWithTimeout(method string, v any, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- s.writeJSON(v)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	ctxDone := s.contextDone()
	select {
	case err := <-done:
		return err
	case <-ctxDone:
		return s.contextErr()
	case <-timer.C:
		err := fmt.Errorf("%s write timed out", method)
		slog.Warn("codex app-server write timed out, closing session", "method", method, "timeout", timeout)
		s.abortTransport()
		return err
	}
}

func (s *appServerSession) contextDone() <-chan struct{} {
	if s.ctx == nil {
		return nil
	}
	return s.ctx.Done()
}

func (s *appServerSession) contextErr() error {
	if s.ctx == nil {
		return context.Canceled
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func (s *appServerSession) abortTransport() {
	s.alive.Store(false)
	if s.cancel != nil {
		s.cancel()
	}

	s.transportMu.Lock()
	conn := s.wsConn
	s.wsConn = nil
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.transportMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (s *appServerSession) notify(method string, params any) error {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	return s.writeJSON(payload)
}

func (s *appServerSession) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("codex app-server encode: %w", err)
	}

	s.transportMu.Lock()
	conn := s.wsConn
	stdin := s.stdin
	s.transportMu.Unlock()
	if conn == nil && stdin == nil {
		return fmt.Errorf("codex app-server connection is closed")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if conn != nil {
		if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
			return fmt.Errorf("codex app-server websocket write: %w", err)
		}
		return nil
	}
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("codex app-server write: %w", err)
	}
	return nil
}
