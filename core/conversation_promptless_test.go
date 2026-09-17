package core

import (
	"os"
	"path/filepath"
	"testing"
)

func promptlessMirrorTest(t *testing.T) (*Engine, *mirrorTestAgent, *mirrorTestPlatform, *conversationMirror, *trackBindingState) {
	t.Helper()
	a := newMirrorTestAgent(mirrorTestSnapshot("thread-1", ConversationTurn{}))
	p := newMirrorTestPlatform()
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(e.cancel)
	b, err := e.bindConversationMirror(p, "mirror:chat:admin", "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	e.sessions.GetOrCreateActive(b.SessionKey).SetAgentSessionID(b.ThreadID, a.Name())
	m := &conversationMirror{destination: b.Destination, sessionKey: b.SessionKey, threadID: b.ThreadID, generation: b.Generation, handles: make(map[string]any)}
	return e, a, p, m, b
}

func TestConversationMirror_TerminalPromptlessTurnDoesNotStall(t *testing.T) {
	for _, status := range []ConversationTurnStatus{ConversationTurnCompleted, ConversationTurnFailed, ConversationTurnInterrupted} {
		t.Run(string(status), func(t *testing.T) {
			e, a, p, m, b := promptlessMirrorTest(t)
			observed := ConversationTurn{ID: "observed", Status: ConversationTurnCompleted}
			if err := e.trackStore.setInitialized(e.trackStore.binding(b.Destination), observed.ID, []string{observed.ID}); err != nil {
				t.Fatal(err)
			}
			shell := ConversationTurn{ID: "shell", Status: status}
			external := ConversationTurn{ID: "external", Status: ConversationTurnCompleted, Messages: []ConversationMessage{{Role: "user", Content: "later external prompt"}}}
			a.setSnapshot(&ConversationSnapshot{SessionID: b.ThreadID, Turns: []ConversationTurn{observed, shell, external}})
			for i := 0; i < 3; i++ {
				if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
					t.Fatal(err)
				}
				if got := e.trackStore.binding(b.Destination); got.Watermark != external.ID || got.LastTurnID != external.ID {
					t.Fatalf("stalled binding: %#v", got)
				}
				if e.trackStore.delivery(b.Destination, b.ThreadID, shell.ID, "primary") != nil {
					t.Fatal("promptless turn claimed a delivery")
				}
				if len(p.starts) != 1 {
					t.Fatalf("cards = %d, want one external card", len(p.starts))
				}
				e.trackStore = newTrackStateStore(e.trackStore.path)
				m.handles = make(map[string]any)
			}
		})
	}
}

func TestConversationMirror_UnresolvedPromptlessTurnWaits(t *testing.T) {
	for _, status := range []ConversationTurnStatus{ConversationTurnInProgress, ConversationTurnUnknown} {
		for _, busy := range []bool{false, true} {
			t.Run(string(status)+map[bool]string{false: "/idle", true: "/busy"}[busy], func(t *testing.T) {
				e, a, p, m, b := promptlessMirrorTest(t)
				if err := e.trackStore.setInitialized(e.trackStore.binding(b.Destination), "", nil); err != nil {
					t.Fatal(err)
				}
				s := e.sessions.GetOrCreateActive(b.SessionKey)
				if busy {
					if !s.TryLock() {
						t.Fatal("session already busy")
					}
					defer s.Unlock()
				}
				turn := ConversationTurn{ID: "unresolved", Status: status}
				later := ConversationTurn{ID: "later", Status: ConversationTurnCompleted, Messages: []ConversationMessage{{Role: "user", Content: "later", ClientID: "external-client"}}}
				a.setSnapshot(&ConversationSnapshot{SessionID: b.ThreadID, Turns: []ConversationTurn{turn, later}})
				if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
					t.Fatal(err)
				}
				if e.trackStore.binding(b.Destination).Watermark != "" || len(p.starts) != 0 {
					t.Fatal("advanced past unresolved turn")
				}
				turn.Status = ConversationTurnCompleted
				a.setSnapshot(&ConversationSnapshot{SessionID: b.ThreadID, Turns: []ConversationTurn{turn, later}})
				if err := e.reconcileConversationMirror(e.ctx, m, a, e.sessions, p); err != nil {
					t.Fatal(err)
				}
				if e.trackStore.binding(b.Destination).Watermark != later.ID || len(p.starts) != 1 {
					t.Fatal("terminal turn still blocks later external delivery")
				}
			})
		}
	}
}

func TestConversationMirror_PromptlessObservationPersistenceFailure(t *testing.T) {
	e, _, p, m, b := promptlessMirrorTest(t)
	if err := e.trackStore.setInitialized(e.trackStore.binding(b.Destination), "before", []string{"before"}); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	e.trackStore.path = filepath.Join(blocked, "state.json")
	turns := []ConversationTurn{{ID: "shell", Status: ConversationTurnCompleted}, {ID: "later", Status: ConversationTurnCompleted, Messages: []ConversationMessage{{Role: "user", Content: "later"}}}}
	err := e.deliverConversationCandidates(e.ctx, m, e.trackStore.binding(b.Destination), &ConversationSnapshot{SessionID: b.ThreadID, Turns: turns}, turns, e.sessions, p)
	if err == nil {
		t.Fatal("expected observation persistence error")
	}
	if e.trackStore.binding(b.Destination).Watermark != "before" || len(p.starts) != 0 {
		t.Fatal("advanced after failed persistence")
	}
}

func TestConversationMirror_PromptlessTurnPreservesKnownSource(t *testing.T) {
	for _, source := range []string{"reservation", "foreground", "external"} {
		t.Run(source, func(t *testing.T) {
			e, _, p, m, b := promptlessMirrorTest(t)
			s := e.sessions.GetOrCreateActive(b.SessionKey)
			if !s.TryLock() {
				t.Fatal("session already busy")
			}
			defer s.Unlock()
			turn := ConversationTurn{ID: "known", Status: ConversationTurnCompleted}
			wantSource := source
			if source == "reservation" {
				wantSource = "foreground"
				if err := e.trackStore.reserveForeground(trackForegroundReservation{
					ClientID: "foreground-client", Destination: b.Destination, SessionKey: b.SessionKey,
					ThreadID: b.ThreadID, TurnID: turn.ID, Generation: b.Generation,
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, _, err := e.trackStore.claimDelivery(b, turn.ID, "primary", source, ""); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				processed, err := e.deliverConversationTurn(e.ctx, m, b, mirrorTestSnapshot(b.ThreadID, turn), turn, e.sessions, p)
				if err != nil || !processed {
					t.Fatalf("processed=%v error=%v", processed, err)
				}
				d := e.trackStore.delivery(b.Destination, b.ThreadID, turn.ID, "primary")
				if d == nil || d.Source != wantSource || !d.Terminal {
					t.Fatalf("delivery = %#v", d)
				}
			}
			wantCards := 0
			if source == "external" {
				wantCards = 1
			}
			if len(p.starts) != wantCards {
				t.Fatalf("cards=%d want=%d", len(p.starts), wantCards)
			}
		})
	}
}

func TestTrackStore_ResetBaselinePreservesLastProcessedTurn(t *testing.T) {
	e, _, _, _, b := promptlessMirrorTest(t)
	if err := e.trackStore.markTurnObserved(e.trackStore.binding(b.Destination), "processed"); err != nil {
		t.Fatal(err)
	}
	if err := e.trackStore.resetBaseline(b.Destination); err != nil {
		t.Fatal(err)
	}
	got := e.trackStore.binding(b.Destination)
	if got.Initialized || got.Watermark != "" || len(got.RecentTurnIDs) != 0 || got.LastTurnID != "processed" {
		t.Fatalf("reset binding = %#v", got)
	}
	if err := e.trackStore.setInitialized(e.trackStore.binding(b.Destination), "baseline", []string{"baseline"}); err != nil {
		t.Fatal(err)
	}
	got = newTrackStateStore(e.trackStore.path).binding(b.Destination)
	if got.Watermark != "baseline" || got.LastTurnID != "processed" {
		t.Fatalf("baseline binding = %#v", got)
	}
}
