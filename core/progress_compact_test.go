package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type suppressTestPlatform struct {
	style string
}

func (s *suppressTestPlatform) Name() string                             { return "test" }
func (s *suppressTestPlatform) Start(MessageHandler) error               { return nil }
func (s *suppressTestPlatform) Reply(context.Context, any, string) error { return nil }
func (s *suppressTestPlatform) Send(context.Context, any, string) error  { return nil }
func (s *suppressTestPlatform) Stop() error                              { return nil }
func (s *suppressTestPlatform) ProgressStyle() string                    { return s.style }

func TestSuppressStandaloneToolResultEvent(t *testing.T) {
	if SuppressStandaloneToolResultEvent(&stubPlatformNoProgress{}) {
		t.Fatal("platform without ProgressStyleProvider should not suppress")
	}
	if !SuppressStandaloneToolResultEvent(&suppressTestPlatform{style: "legacy"}) {
		t.Fatal("legacy ProgressStyleProvider should suppress standalone tool results")
	}
	if SuppressStandaloneToolResultEvent(&suppressTestPlatform{style: "compact"}) {
		t.Fatal("compact should not suppress (writer absorbs tool results)")
	}
	if SuppressStandaloneToolResultEvent(&suppressTestPlatform{style: "card"}) {
		t.Fatal("card should not suppress")
	}
}

// stubPlatformNoProgress is a minimal Platform without ProgressStyleProvider.
type stubPlatformNoProgress struct{}

func (stubPlatformNoProgress) Name() string                             { return "plain" }
func (stubPlatformNoProgress) Start(MessageHandler) error               { return nil }
func (stubPlatformNoProgress) Reply(context.Context, any, string) error { return nil }
func (stubPlatformNoProgress) Send(context.Context, any, string) error  { return nil }
func (stubPlatformNoProgress) Stop() error                              { return nil }

type progressHintReplyCtx struct {
	style   string
	payload bool
}

func (r progressHintReplyCtx) progressStyleHint() string { return r.style }

func (r progressHintReplyCtx) supportsProgressCardPayloadHint() bool { return r.payload }

type previewCapturePlatform struct {
	started []string
	updated []string
}

func (p *previewCapturePlatform) Name() string                             { return "bridge" }
func (p *previewCapturePlatform) Start(MessageHandler) error               { return nil }
func (p *previewCapturePlatform) Reply(context.Context, any, string) error { return nil }
func (p *previewCapturePlatform) Send(context.Context, any, string) error  { return nil }
func (p *previewCapturePlatform) Stop() error                              { return nil }

func (p *previewCapturePlatform) SendPreviewStart(_ context.Context, _ any, content string) (any, error) {
	p.started = append(p.started, content)
	return "preview-1", nil
}

func (p *previewCapturePlatform) UpdateMessage(_ context.Context, _ any, content string) error {
	p.updated = append(p.updated, content)
	return nil
}

func TestBuildAndParseProgressCardPayload(t *testing.T) {
	payload := BuildProgressCardPayload([]string{" step1 ", "", "step2"}, true)
	if payload == "" {
		t.Fatal("BuildProgressCardPayload returned empty string")
	}
	if !strings.HasPrefix(payload, ProgressCardPayloadPrefix) {
		t.Fatalf("payload = %q, want prefix %q", payload, ProgressCardPayloadPrefix)
	}

	parsed, ok := ParseProgressCardPayload(payload)
	if !ok {
		t.Fatalf("ParseProgressCardPayload should succeed, payload=%q", payload)
	}
	if len(parsed.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(parsed.Entries))
	}
	if parsed.Entries[0] != "step1" || parsed.Entries[1] != "step2" {
		t.Fatalf("entries = %#v, want [step1 step2]", parsed.Entries)
	}
	if !parsed.Truncated {
		t.Fatal("parsed.Truncated = false, want true")
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(parsed.Items))
	}
	if parsed.Items[0].Kind != ProgressEntryInfo || parsed.Items[0].Text != "step1" {
		t.Fatalf("items[0] = %#v, want info/step1", parsed.Items[0])
	}
}

func TestCompactProgressWriter_UsesReplyContextHints(t *testing.T) {
	p := &previewCapturePlatform{}
	replyCtx := progressHintReplyCtx{
		style:   progressStyleCard,
		payload: true,
	}

	w := newCompactProgressWriter(context.Background(), p, replyCtx, "codex", LangEnglish, nil)
	if !w.enabled {
		t.Fatal("progress writer should be enabled")
	}
	if !w.usePayload {
		t.Fatal("progress writer should use payload when reply context advertises it")
	}
	if got := w.style; got != progressStyleCard {
		t.Fatalf("style = %q, want %q", got, progressStyleCard)
	}

	if !w.AppendEvent(ProgressEntryThinking, "planning bridge progress", "", "planning bridge progress") {
		t.Fatal("AppendEvent() = false, want true")
	}
	if len(p.started) != 1 {
		t.Fatalf("started = %d, want 1", len(p.started))
	}
	if !strings.HasPrefix(p.started[0], ProgressCardPayloadPrefix) {
		t.Fatalf("preview start payload = %q, want progress payload prefix", p.started[0])
	}

	if !w.Finalize(ProgressCardStateCompleted) {
		t.Fatal("Finalize() = false, want true")
	}
	if len(p.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(p.updated))
	}

	parsed, ok := ParseProgressCardPayload(p.updated[0])
	if !ok {
		t.Fatalf("ParseProgressCardPayload() failed for %q", p.updated[0])
	}
	if parsed.State != ProgressCardStateCompleted {
		t.Fatalf("state = %q, want %q", parsed.State, ProgressCardStateCompleted)
	}
}

func TestBuildAndParseProgressCardPayloadV2(t *testing.T) {
	payload := BuildProgressCardPayloadV2([]ProgressCardEntry{
		{Kind: ProgressEntryThinking, Text: " plan "},
		{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "pwd"},
	}, false, "Codex", LangChinese, ProgressCardStateRunning)
	if payload == "" {
		t.Fatal("BuildProgressCardPayloadV2 returned empty string")
	}

	parsed, ok := ParseProgressCardPayload(payload)
	if !ok {
		t.Fatalf("ParseProgressCardPayload should succeed, payload=%q", payload)
	}
	if parsed.Version != 2 {
		t.Fatalf("version = %d, want 2", parsed.Version)
	}
	if parsed.Agent != "Codex" {
		t.Fatalf("agent = %q, want Codex", parsed.Agent)
	}
	if parsed.Lang != string(LangChinese) {
		t.Fatalf("lang = %q, want %q", parsed.Lang, LangChinese)
	}
	if parsed.State != ProgressCardStateRunning {
		t.Fatalf("state = %q, want %q", parsed.State, ProgressCardStateRunning)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(parsed.Items))
	}
	if parsed.Items[1].Kind != ProgressEntryToolUse || parsed.Items[1].Tool != "Bash" {
		t.Fatalf("items[1] = %#v, want tool_use/Bash", parsed.Items[1])
	}
}

func TestCompactProgressWriter_ControlsAppearOnlyWhileRunning(t *testing.T) {
	p := &previewCapturePlatform{}
	replyCtx := progressHintReplyCtx{style: progressStyleCard, payload: true}
	w := newCompactProgressWriter(context.Background(), p, replyCtx, "codex", LangChinese, nil)

	handles := make([]any, 0, 1)
	w.SetHandleObserver(func(handle any) { handles = append(handles, handle) })
	if !w.AppendEvent(ProgressEntryThinking, "检查中", "", "检查中") {
		t.Fatal("AppendEvent() = false, want true")
	}
	if len(handles) != 1 || handles[0] != "preview-1" {
		t.Fatalf("observed handles = %#v", handles)
	}
	button := CardButton{Text: "中止当前任务", Type: "danger", Value: "turn:interrupt:token", Extra: map[string]string{"session_key": "feishu:chat:user"}}
	if !w.SetControls("回复此卡片可向当前任务追加指令。", []CardButton{button}) {
		t.Fatal("SetControls() = false, want true")
	}
	if len(p.updated) != 1 {
		t.Fatalf("control updates = %d, want 1", len(p.updated))
	}
	running, ok := ParseProgressCardPayload(p.updated[0])
	if !ok {
		t.Fatalf("control payload did not parse: %q", p.updated[0])
	}
	if running.Hint == "" || len(running.Buttons) != 1 || running.Buttons[0].Value != button.Value {
		t.Fatalf("running controls = hint %q buttons %#v", running.Hint, running.Buttons)
	}

	if !w.Finalize(ProgressCardStateInterrupted) {
		t.Fatal("Finalize(interrupted) = false, want true")
	}
	terminal, ok := ParseProgressCardPayload(p.updated[len(p.updated)-1])
	if !ok {
		t.Fatalf("terminal payload did not parse: %q", p.updated[len(p.updated)-1])
	}
	if terminal.State != ProgressCardStateInterrupted || terminal.Hint != "" || len(terminal.Buttons) != 0 {
		t.Fatalf("terminal payload = state %q hint %q buttons %#v", terminal.State, terminal.Hint, terminal.Buttons)
	}
}

func TestProgressCardStateFromResult(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata map[string]any
		want     ProgressCardState
	}{
		{name: "default completed", want: ProgressCardStateCompleted},
		{name: "completed", metadata: map[string]any{"turn_status": "completed"}, want: ProgressCardStateCompleted},
		{name: "failed", metadata: map[string]any{"turn_status": "failed"}, want: ProgressCardStateFailed},
		{name: "cancelled aliases interrupted", metadata: map[string]any{"turn_status": "cancelled"}, want: ProgressCardStateInterrupted},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := progressCardStateFromResult(Event{Metadata: test.metadata}); got != test.want {
				t.Fatalf("progressCardStateFromResult() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseProgressCardPayloadRejectsInvalid(t *testing.T) {
	if _, ok := ParseProgressCardPayload("plain text"); ok {
		t.Fatal("expected parse failure for plain text")
	}
	if _, ok := ParseProgressCardPayload(ProgressCardPayloadPrefix + "{not-json"); ok {
		t.Fatal("expected parse failure for invalid json")
	}
	if _, ok := ParseProgressCardPayload(ProgressCardPayloadPrefix + `{"entries":[]}`); ok {
		t.Fatal("expected parse failure for empty entries")
	}
}

func TestCompactProgressWriter_AppliesTransformToCardPayloadEntries(t *testing.T) {
	p := &stubCompactProgressPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "feishu"},
		style:              "card",
		supportPayload:     true,
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "codex", LangEnglish, func(s string) string {
		return strings.ReplaceAll(s, "/root/code/demo/src/app.ts:42", "📄 `src/app.ts:42`")
	})

	if ok := w.AppendStructured(ProgressCardEntry{
		Kind: ProgressEntryThinking,
		Text: "Inspect /root/code/demo/src/app.ts:42",
	}, "Inspect /root/code/demo/src/app.ts:42"); !ok {
		t.Fatal("AppendStructured() = false, want true")
	}

	starts := p.getPreviewStarts()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want 1", len(starts))
	}
	payload, ok := ParseProgressCardPayload(starts[0])
	if !ok {
		t.Fatalf("ParseProgressCardPayload(%q) failed", starts[0])
	}
	if len(payload.Items) != 1 {
		t.Fatalf("payload items = %d, want 1", len(payload.Items))
	}
	if got := payload.Items[0].Text; got != "Inspect 📄 `src/app.ts:42`" {
		t.Fatalf("payload item text = %q, want transformed text", got)
	}
}

type stubThrottledProgressPlatform struct {
	stubCompactProgressPlatform
	throttle time.Duration
}

func (p *stubThrottledProgressPlatform) ProgressUpdateInterval() time.Duration {
	return p.throttle
}

type stubHeartbeatProgressPlatform struct {
	stubCompactProgressPlatform
	heartbeat time.Duration
}

func (p *stubHeartbeatProgressPlatform) ProgressHeartbeatInterval() time.Duration {
	return p.heartbeat
}

func waitForProgressEdits(t *testing.T, p *stubCompactProgressPlatform, count int) []string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		edits := p.getPreviewEdits()
		if len(edits) >= count {
			return edits
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("preview edits = %d, want at least %d", len(p.getPreviewEdits()), count)
	return nil
}

func TestCompactProgressWriter_ThrottlesRapidUpdates(t *testing.T) {
	p := &stubThrottledProgressPlatform{
		stubCompactProgressPlatform: stubCompactProgressPlatform{
			stubPlatformEngine: stubPlatformEngine{n: "discord"},
			style:              "card",
			supportPayload:     true,
		},
		throttle: 50 * time.Millisecond,
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "cc", LangEnglish, nil)

	w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryThinking, Text: "step 1"}, "step 1")
	if len(p.getPreviewStarts()) != 1 {
		t.Fatal("first update should create the preview message")
	}

	w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "pwd"}, "pwd")
	w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolResult, Tool: "Bash", Text: "ok"}, "ok")
	editsBeforeThrottle := len(p.getPreviewEdits())
	if editsBeforeThrottle > 0 {
		t.Fatalf("rapid updates within throttle window should be skipped, got %d edits", editsBeforeThrottle)
	}

	time.Sleep(60 * time.Millisecond)
	w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryThinking, Text: "step 4"}, "step 4")
	editsAfterWait := len(p.getPreviewEdits())
	if editsAfterWait != 1 {
		t.Fatalf("update after throttle interval should go through, got %d edits", editsAfterWait)
	}

	ok := w.Finalize(ProgressCardStateCompleted)
	if !ok {
		t.Fatal("Finalize should succeed")
	}
	finalEdits := p.getPreviewEdits()
	last := finalEdits[len(finalEdits)-1]
	payload, parsed := ParseProgressCardPayload(last)
	if !parsed {
		t.Fatalf("final edit should be a valid payload, got %q", last)
	}
	if payload.State != ProgressCardStateCompleted {
		t.Fatalf("state = %q, want completed", payload.State)
	}
	if len(payload.Items) != 4 {
		t.Fatalf("items = %d, want 4 (all buffered items)", len(payload.Items))
	}
}

func TestCompactProgressWriter_ThrottledUpdateFlushesWithoutAnotherEvent(t *testing.T) {
	p := &stubThrottledProgressPlatform{
		stubCompactProgressPlatform: stubCompactProgressPlatform{
			stubPlatformEngine: stubPlatformEngine{n: "feishu"},
			style:              "card",
			supportPayload:     true,
		},
		throttle: 30 * time.Millisecond,
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "codex", LangEnglish, nil)
	t.Cleanup(w.Close)

	w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryThinking, Text: "step 1"}, "step 1")
	w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "sleep 60"}, "sleep 60")

	edits := waitForProgressEdits(t, &p.stubCompactProgressPlatform, 1)
	payload, ok := ParseProgressCardPayload(edits[len(edits)-1])
	if !ok {
		t.Fatalf("trailing edit should be a valid payload: %q", edits[len(edits)-1])
	}
	if len(payload.Items) != 2 || payload.Counts.Reasoning != 1 || payload.Counts.Tools != 1 {
		t.Fatalf("trailing payload = items %#v counts %#v", payload.Items, payload.Counts)
	}
}

func TestCompactProgressWriter_CumulativeCountsSurviveVisibleEntryLimit(t *testing.T) {
	p := &stubCompactProgressPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "feishu"},
		style:              "card",
		supportPayload:     true,
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "codex", LangChinese, nil)
	t.Cleanup(w.Close)

	for i := 0; i < 17; i++ {
		text := fmt.Sprintf("command-%02d", i)
		if !w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: text}, text) {
			t.Fatalf("AppendStructured(%d) = false", i)
		}
	}
	edits := p.getPreviewEdits()
	payload, ok := ParseProgressCardPayload(edits[len(edits)-1])
	if !ok {
		t.Fatalf("last edit should be a valid payload: %q", edits[len(edits)-1])
	}
	if !payload.Truncated || len(payload.Items) != 10 {
		t.Fatalf("visible progress = %d, truncated = %t", len(payload.Items), payload.Truncated)
	}
	if payload.Counts.Tools != 17 {
		t.Fatalf("cumulative tool count = %d, want 17", payload.Counts.Tools)
	}
	if got := payload.Items[0].Text; got != "command-07" {
		t.Fatalf("first visible item = %q, want command-07", got)
	}
}

func TestCompactProgressWriter_HeartbeatRefreshesLongSilentOperation(t *testing.T) {
	p := &stubHeartbeatProgressPlatform{
		stubCompactProgressPlatform: stubCompactProgressPlatform{
			stubPlatformEngine: stubPlatformEngine{n: "feishu"},
			style:              "card",
			supportPayload:     true,
		},
		heartbeat: 20 * time.Millisecond,
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "codex", LangEnglish, nil)
	t.Cleanup(w.Close)
	if !w.AppendStructured(ProgressCardEntry{Kind: ProgressEntryToolUse, Tool: "Bash", Text: "sleep 60"}, "sleep 60") {
		t.Fatal("initial progress append failed")
	}
	w.mu.Lock()
	w.startedAt = time.Now().Add(-2 * time.Second)
	w.mu.Unlock()

	edits := waitForProgressEdits(t, &p.stubCompactProgressPlatform, 1)
	payload, ok := ParseProgressCardPayload(edits[len(edits)-1])
	if !ok {
		t.Fatalf("heartbeat edit should be a valid payload: %q", edits[len(edits)-1])
	}
	if payload.State != ProgressCardStateRunning || payload.ElapsedSeconds < 2 {
		t.Fatalf("heartbeat payload state = %q elapsed = %d", payload.State, payload.ElapsedSeconds)
	}
}

func TestCompactProgressWriter_DoesNotTransformToolResults(t *testing.T) {
	p := &stubCompactProgressPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "feishu"},
		style:              "card",
		supportPayload:     true,
	}
	w := newCompactProgressWriter(context.Background(), p, "ctx", "codex", LangEnglish, func(s string) string {
		return strings.ReplaceAll(s, "/root/code/demo/src/app.ts:42", "📄 `src/app.ts:42`")
	})

	raw := "/root/code/demo/src/app.ts:42"
	if ok := w.AppendStructured(ProgressCardEntry{
		Kind: ProgressEntryToolResult,
		Text: raw,
	}, raw); !ok {
		t.Fatal("AppendStructured() = false, want true")
	}

	starts := p.getPreviewStarts()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want 1", len(starts))
	}
	payload, ok := ParseProgressCardPayload(starts[0])
	if !ok {
		t.Fatalf("ParseProgressCardPayload(%q) failed", starts[0])
	}
	if got := payload.Items[0].Text; got != raw {
		t.Fatalf("tool result text = %q, want raw %q", got, raw)
	}
}
