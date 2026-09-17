package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func rollbackSnapshot(turns ...ConversationTurn) *ConversationSnapshot {
	return &ConversationSnapshot{SessionID: "thread-1", Turns: turns, HistoryComplete: true}
}

func rollbackTurn(id string, status ConversationTurnStatus) ConversationTurn {
	return ConversationTurn{ID: id, Status: status, Messages: []ConversationMessage{
		{Role: "user", Content: "prompt " + id},
		{Role: "assistant", Content: "result " + id, Phase: "final_answer"},
	}}
}

type rollbackReadAgent struct {
	*mirrorTestAgent
	afterRead func()
}

func (a *rollbackReadAgent) GetConversationWindow(ctx context.Context, threadID, watermark string, limit int) (*ConversationSnapshot, bool, error) {
	snapshot, covered, err := a.mirrorTestAgent.GetConversationWindow(ctx, threadID, watermark, limit)
	a.afterRead()
	return snapshot, covered, err
}

func TestConversationMirror_RollbackRecovery(t *testing.T) {
	t.Run("replacement survives reconciliation and restart", func(t *testing.T) {
		e, a, p, m, b := promptlessMirrorTest(t)
		reconcile := func(turns ...ConversationTurn) {
			t.Helper()
			a.setSnapshot(rollbackSnapshot(turns...))
			if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
				t.Fatal(err)
			}
		}
		first := rollbackTurn("A", ConversationTurnCompleted)
		removed := rollbackTurn("B", ConversationTurnInterrupted)
		replacement := rollbackTurn("C", ConversationTurnCompleted)
		later := rollbackTurn("D", ConversationTurnCompleted)
		reconcile()
		reconcile(first)
		reconcile(first, removed)
		reconcile(first, replacement)
		if got := e.trackStore.binding(b.Destination); got.Watermark != "B" || len(p.starts) != 2 {
			t.Fatalf("first missing-watermark read changed progress: %#v, cards=%d", got, len(p.starts))
		}
		reconcile(first, replacement, later)
		if got := e.trackStore.binding(b.Destination); got.Watermark != "D" || got.Gap != "" {
			t.Fatalf("rollback stalled mirroring: %#v", got)
		}
		for i := 0; i < 3; i++ {
			e.trackStore = newTrackStateStore(e.trackStore.path)
			m.handles = make(map[string]any)
			reconcile(first, replacement, later)
		}
		if len(p.starts) != 4 || len(p.notificationKey) != 4 {
			t.Fatalf("duplicate deliveries: cards=%d results=%d", len(p.starts), len(p.notificationKey))
		}
		for _, id := range []string{"A", "B", "C", "D"} {
			if got := strings.Count(strings.Join(p.getSent(), "\n"), "result "+id); got != 1 {
				t.Fatalf("result %s delivered %d times", id, got)
			}
		}
	})

	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "active refresh preserves newer checkpoint", true: "active refresh preserves missing watermark"}[missing], func(t *testing.T) {
			e, a, p, m, b := promptlessMirrorTest(t)
			first := rollbackTurn("A", ConversationTurnInProgress)
			removed := rollbackTurn("B", ConversationTurnInterrupted)
			for _, snapshot := range []*ConversationSnapshot{rollbackSnapshot(), rollbackSnapshot(first), rollbackSnapshot(first, removed)} {
				a.setSnapshot(snapshot)
				if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
					t.Fatal(err)
				}
			}
			first.Status = ConversationTurnCompleted
			snapshot := rollbackSnapshot(first)
			if !missing {
				snapshot.Turns = append(snapshot.Turns, removed)
			}
			a.setSnapshot(snapshot)
			if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
				t.Fatal(err)
			}
			if got := e.trackStore.binding(b.Destination); got.Watermark != "B" || (got.Gap != "") != missing {
				t.Fatalf("card refresh moved the checkpoint: %#v", got)
			}
			if d := e.trackStore.delivery(b.Destination, b.ThreadID, "A", "primary"); d == nil || !d.Terminal {
				t.Fatalf("surviving card was not refreshed: %#v", d)
			}
		})
	}

	for _, scenario := range []string{"multiple removed turns", "legacy live baseline", "history order selects newest anchor", "already delivered successor", "foreground successor", "ongoing successor"} {
		t.Run(scenario, func(t *testing.T) {
			e, a, p, m, b := promptlessMirrorTest(t)
			first := rollbackTurn("A", ConversationTurnCompleted)
			successor := rollbackTurn("C", ConversationTurnCompleted)
			recent, watermark := []string{"A", "B"}, "B"
			if scenario == "multiple removed turns" {
				recent, watermark = []string{"A", "B", "removed-2"}, "removed-2"
			}
			if scenario == "legacy live baseline" {
				recent = append(recent, "C")
			}
			if scenario == "history order selects newest anchor" {
				recent = []string{"A2", "A", "B"}
			}
			if err := e.trackStore.setInitialized(b, watermark, recent); err != nil {
				t.Fatal(err)
			}
			b = e.trackStore.binding(b.Destination)
			wantCards := 1
			if scenario == "foreground successor" {
				wantCards = 0
				successor.Messages[0].ClientID = "foreground-C"
				if err := e.trackStore.reserveForeground(trackForegroundReservation{
					ClientID: "foreground-C", Destination: b.Destination, SessionKey: b.SessionKey,
					ThreadID: b.ThreadID, Generation: b.Generation,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "ongoing successor" {
				successor.Status = ConversationTurnInProgress
			}
			snapshot := rollbackSnapshot(first, successor)
			wantRecent := []string{"A", "C"}
			if scenario == "history order selects newest anchor" {
				snapshot = rollbackSnapshot(first, rollbackTurn("A2", ConversationTurnCompleted), successor)
				wantRecent = []string{"A", "A2", "C"}
			}
			if scenario == "already delivered successor" {
				if _, err := e.deliverConversationTurn(e.ctx, m, b, snapshot, successor, e.sessions, p); err != nil {
					t.Fatal(err)
				}
			}
			a.setSnapshot(snapshot)
			for i := 0; i < 3; i++ {
				if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
					t.Fatal(err)
				}
			}
			if got := e.trackStore.binding(b.Destination); got.Watermark != "C" || got.Gap != "" || !reflect.DeepEqual(got.RecentTurnIDs, wantRecent) {
				t.Fatalf("recovered checkpoint = %#v", got)
			}
			if scenario == "ongoing successor" {
				if len(p.getSent()) != 0 {
					t.Fatal("running successor sent a final result")
				}
				successor.Status = ConversationTurnCompleted
				a.setSnapshot(rollbackSnapshot(first, successor))
				if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
					t.Fatal(err)
				}
			}
			if len(p.starts) != wantCards || len(p.notificationKey) != wantCards {
				t.Fatalf("cards=%d results=%d, want %d", len(p.starts), len(p.notificationKey), wantCards)
			}
		})
	}

	t.Run("baseline anchor and restart before successor processed", func(t *testing.T) {
		e, a, p, m, b := promptlessMirrorTest(t)
		first := rollbackTurn("A", ConversationTurnCompleted)
		a.setSnapshot(rollbackSnapshot(first, rollbackTurn("B", ConversationTurnCompleted)))
		if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
			t.Fatal(err)
		}
		if len(p.starts) != 0 {
			t.Fatal("baseline replayed completed turns")
		}
		a.setSnapshot(rollbackSnapshot(first, ConversationTurn{ID: "C", Status: ConversationTurnInProgress}))
		for i := 0; i < 2; i++ {
			if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.trackStore.binding(b.Destination); got.Watermark != "A" || got.LastTurnID != "" || got.Gap != "" {
			t.Fatalf("rewind did not persist independently of delivery: %#v", got)
		}
		e.trackStore = newTrackStateStore(e.trackStore.path)
		m = &conversationMirror{destination: b.Destination, sessionKey: b.SessionKey, threadID: b.ThreadID, generation: b.Generation, handles: make(map[string]any)}
		a.setSnapshot(rollbackSnapshot(first, rollbackTurn("C", ConversationTurnCompleted)))
		for i := 0; i < 2; i++ {
			if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.trackStore.binding(b.Destination); got.Watermark != "C" || got.LastTurnID != "C" || len(p.starts) != 1 || len(p.notificationKey) != 1 {
			t.Fatalf("restart lost or duplicated replacement: %#v, cards=%d results=%d", got, len(p.starts), len(p.notificationKey))
		}
	})

	for _, interruption := range []string{"incomplete", "read failure", "different history", "watermark returns", "stale prefix", "restart", "wrong thread", "duplicate IDs"} {
		t.Run("confirmation reset/"+interruption, func(t *testing.T) {
			e, a, p, m, b := promptlessMirrorTest(t)
			if err := e.trackStore.setInitialized(b, "B", []string{"A", "B"}); err != nil {
				t.Fatal(err)
			}
			first, next := rollbackTurn("A", ConversationTurnCompleted), rollbackTurn("C", ConversationTurnCompleted)
			a.setSnapshot(rollbackSnapshot(first, next))
			if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
				t.Fatal(err)
			}
			changed := rollbackSnapshot(first, next)
			wantError := false
			switch interruption {
			case "incomplete":
				changed.HistoryComplete = false
			case "read failure":
				a.readErr = errors.New("read unavailable")
				wantError = true
			case "different history":
				changed = rollbackSnapshot(first, rollbackTurn("different", ConversationTurnCompleted))
			case "watermark returns":
				changed = rollbackSnapshot(first, rollbackTurn("B", ConversationTurnInterrupted))
			case "stale prefix":
				changed = rollbackSnapshot(first)
			case "restart":
				e.trackStore = newTrackStateStore(e.trackStore.path)
				m = &conversationMirror{destination: b.Destination, sessionKey: b.SessionKey, threadID: b.ThreadID, generation: b.Generation, handles: make(map[string]any)}
			case "wrong thread":
				changed.SessionID = "another-thread"
				wantError = true
			case "duplicate IDs":
				changed.Turns = append(changed.Turns, next)
				wantError = true
			}
			a.setSnapshot(changed)
			err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p)
			if (err != nil) != wantError {
				t.Fatalf("interruption error = %v", err)
			}
			if got := e.trackStore.binding(b.Destination); got.Watermark != "B" || len(p.starts) != 0 {
				t.Fatalf("unconfirmed history advanced: %#v, cards=%d", got, len(p.starts))
			}
			a.readErr = nil
			a.setSnapshot(rollbackSnapshot(first, next))
			if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
				t.Fatal(err)
			}
			if interruption != "restart" && len(p.starts) != 0 {
				t.Fatal("recovery reused confirmation from before interruption")
			}
			if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
				t.Fatal(err)
			}
			if len(p.starts) != 1 || e.trackStore.binding(b.Destination).Watermark != "C" {
				t.Fatal("fresh consecutive reads did not recover")
			}
		})
	}

	for _, scenario := range []string{"no common anchor", "empty history", "only legacy unprocessed ID", "stale prefix", "incomplete history"} {
		t.Run(scenario, func(t *testing.T) {
			e, a, p, m, b := promptlessMirrorTest(t)
			recent := []string{"A", "B"}
			snapshot := rollbackSnapshot(rollbackTurn("C", ConversationTurnCompleted))
			wantGap := "rollback_no_anchor"
			switch scenario {
			case "empty history":
				snapshot = rollbackSnapshot()
			case "only legacy unprocessed ID":
				recent = append(recent, "C")
			case "stale prefix":
				snapshot = rollbackSnapshot(rollbackTurn("A", ConversationTurnCompleted))
				wantGap = "rollback_pending"
			case "incomplete history":
				snapshot = rollbackSnapshot(rollbackTurn("A", ConversationTurnCompleted), rollbackTurn("C", ConversationTurnCompleted))
				snapshot.HistoryComplete = false
				wantGap = "watermark_not_covered"
			}
			if err := e.trackStore.setInitialized(b, "B", recent); err != nil {
				t.Fatal(err)
			}
			a.setSnapshot(snapshot)
			for i := 0; i < 3; i++ {
				if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
					t.Fatal(err)
				}
			}
			if got := e.trackStore.binding(b.Destination); got.Watermark != "B" || got.Gap != wantGap || len(p.starts) != 0 {
				t.Fatalf("unsafe recovery: %#v, cards=%d", got, len(p.starts))
			}
			if len(p.getSent()) != 0 {
				t.Fatal("blocked recovery sent an unsolicited warning")
			}
		})
	}

	for _, change := range []string{"rebind", "disable", "cancel"} {
		t.Run("during read/"+change, func(t *testing.T) {
			e, a, p, m, b := promptlessMirrorTest(t)
			if err := e.trackStore.setInitialized(b, "B", []string{"A", "B"}); err != nil {
				t.Fatal(err)
			}
			a.setSnapshot(rollbackSnapshot(rollbackTurn("A", ConversationTurnCompleted), rollbackTurn("C", ConversationTurnCompleted)))
			if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			var after *trackBindingState
			reader := &rollbackReadAgent{mirrorTestAgent: a, afterRead: func() {
				var err error
				switch change {
				case "rebind":
					_, err = e.trackStore.bind(b.Destination, b.SessionKey, b.Platform, "thread-new")
				case "disable":
					_, err = e.trackStore.setOverride(b.Destination, b.SessionKey, b.Platform, trackOverrideOff)
				case "cancel":
					cancel()
				}
				if err != nil {
					t.Fatal(err)
				}
				after = e.trackStore.binding(b.Destination)
			}}
			if err := e.reconcileConversationMirror(ctx, m, reader, e.sessions, p); err == nil {
				t.Fatal("stale read was accepted")
			}
			if got := e.trackStore.binding(b.Destination); !reflect.DeepEqual(got, after) || len(p.starts) != 0 {
				t.Fatalf("stale read changed binding or delivered: %#v, cards=%d", got, len(p.starts))
			}
		})
	}

	t.Run("rewind persistence failure", func(t *testing.T) {
		e, a, p, m, b := promptlessMirrorTest(t)
		if err := e.trackStore.setInitialized(b, "B", []string{"A", "B"}); err != nil {
			t.Fatal(err)
		}
		a.setSnapshot(rollbackSnapshot(rollbackTurn("A", ConversationTurnCompleted), rollbackTurn("C", ConversationTurnCompleted)))
		if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
			t.Fatal(err)
		}
		before := e.trackStore.binding(b.Destination)
		originalPath := e.trackStore.path
		beforeData, err := os.ReadFile(originalPath)
		if err != nil {
			t.Fatal(err)
		}
		blocked := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		e.trackStore.path = filepath.Join(blocked, "track.json")
		if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err == nil {
			t.Fatal("rewind succeeded without persistence")
		}
		if !reflect.DeepEqual(e.trackStore.binding(b.Destination), before) || len(p.starts) != 0 {
			t.Fatal("failed rewind changed memory or delivered a turn")
		}
		if afterData, err := os.ReadFile(originalPath); err != nil || string(afterData) != string(beforeData) {
			t.Fatalf("failed rewind changed persisted state: %v", err)
		}
	})

	t.Run("baseline excludes unprocessed active turns", func(t *testing.T) {
		e, a, _, m, b := promptlessMirrorTest(t)
		p := newMirrorTestPlatform()
		s := e.sessions.GetOrCreateActive(b.SessionKey)
		if !s.TryLock() {
			t.Fatal("session already busy")
		}
		defer s.Unlock()
		a.setSnapshot(rollbackSnapshot(rollbackTurn("A", ConversationTurnCompleted), rollbackTurn("B", ConversationTurnInProgress)))
		if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
			t.Fatal(err)
		}
		if got := e.trackStore.binding(b.Destination); got.Watermark != "A" || !reflect.DeepEqual(got.RecentTurnIDs, []string{"A"}) {
			t.Fatalf("unprocessed active turn became a baseline anchor: %#v", got)
		}
	})
}

func TestTrackStateStore_RewindWatermark(t *testing.T) {
	for _, stale := range []string{"", "destination", "session", "thread", "generation", "watermark", "initialized", "override"} {
		t.Run("expected binding/"+stale, func(t *testing.T) {
			store := newTrackStateStore(filepath.Join(t.TempDir(), "track.json"))
			b, err := store.bind("mirror:chat", "mirror:chat:admin", "mirror", "thread-1")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.setInitialized(b, "B", []string{"A", "B"}); err != nil {
				t.Fatal(err)
			}
			if err := store.markTurnObserved(store.binding(b.Destination), "B"); err != nil {
				t.Fatal(err)
			}
			b = store.binding(b.Destination)
			if _, _, err := store.claimDelivery(b, "B", "primary", "external", ""); err != nil {
				t.Fatal(err)
			}
			before := store.binding(b.Destination)
			switch stale {
			case "destination":
				b.Destination = "mirror:other"
			case "session":
				b.SessionKey = "mirror:chat:other"
			case "thread":
				b.ThreadID = "thread-other"
			case "generation":
				b.Generation++
			case "watermark":
				b.Watermark = "old-watermark"
			case "initialized":
				b.Initialized = false
			case "override":
				b.Override = trackOverrideOff
			}
			err = store.rewindWatermark(b, "A", []string{"A"})
			if stale != "" {
				if err == nil || !reflect.DeepEqual(store.binding(before.Destination), before) {
					t.Fatalf("stale rewind changed the binding: %v", err)
				}
				if !trackBindingIdentityMatches(before, b) {
					if _, _, err := store.claimDelivery(b, "C", "primary", "external", ""); !errors.Is(err, errTrackBindingChanged) {
						t.Fatalf("stale delivery claim = %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			reloaded := newTrackStateStore(store.path)
			got := reloaded.binding(b.Destination)
			if got.Watermark != "A" || got.LastTurnID != "B" || !reflect.DeepEqual(got.RecentTurnIDs, []string{"A"}) {
				t.Fatalf("rewound checkpoint = %#v", got)
			}
			if reloaded.delivery(b.Destination, b.ThreadID, "B", "primary") == nil {
				t.Fatal("rewind removed the delivery record")
			}
		})
	}
}

func TestCmdTrack_RollbackGapGuidance(t *testing.T) {
	for _, gap := range []string{"watermark_not_covered", "rollback_pending", "rollback_no_anchor"} {
		for _, command := range []string{"/track", "/track status"} {
			t.Run(gap+command, func(t *testing.T) {
				e, a, p, _, b := promptlessMirrorTest(t)
				e.SetAdminFrom("admin")
				a.setSnapshot(rollbackSnapshot(rollbackTurn("C", ConversationTurnCompleted)))
				if err := e.trackStore.setGap(b, gap); err != nil {
					t.Fatal(err)
				}
				e.ReceiveMessage(p, &Message{SessionKey: b.SessionKey, Platform: p.Name(), UserID: "admin", MessageID: "status", Content: command, ReplyCtx: "ctx"})
				got := strings.Join(p.getSent(), "\n")
				if !strings.Contains(got, "mirroring is paused") || !strings.Contains(got, "retry automatically") {
					t.Fatalf("missing recovery guidance: %q", got)
				}
				if gap == "rollback_no_anchor" && (!strings.Contains(got, "/track off then /track on") || !strings.Contains(got, "will not be replayed")) {
					t.Fatalf("missing reset tradeoff: %q", got)
				}
				if got := e.trackStore.binding(b.Destination); got.Gap != gap {
					t.Fatalf("snapshot refresh cleared mirror gap: %#v", got)
				}
			})
		}
	}
}
