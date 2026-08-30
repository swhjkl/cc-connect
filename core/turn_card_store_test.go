package core

import (
	"path/filepath"
	"sync"
	"testing"
)

func testTurnCardState(token string) turnCardState {
	return turnCardState{
		Token: token, Platform: "feishu", SessionKey: "feishu:chat:user", InteractiveKey: "workspace:feishu:chat:user",
		Destination: "feishu:chat", ThreadID: "thread-1", TurnID: "turn-1", Generation: 1, CardMessageID: "om-card-1",
	}
}

func TestTurnCardStore_ByTurnMatchesStableDestination(t *testing.T) {
	store := newTurnCardStore("")
	card := testTurnCardState("token-destination")
	if err := store.register(card); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if got := store.byTurn("feishu", "feishu:chat:other-user", "feishu:chat", "thread-1", "turn-1"); got == nil || got.Token != card.Token {
		t.Fatalf("same-destination lookup = %#v", got)
	}
	if got := store.byTurn("feishu", "feishu:other:other-user", "feishu:other", "thread-1", "turn-1"); got != nil {
		t.Fatalf("cross-destination lookup = %#v", got)
	}
}

func TestTurnCardStore_RestartMakesActiveCardStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json.turn-cards.json")
	store := newTurnCardStore(path)
	card := testTurnCardState("token-1")
	if err := store.register(card); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if got := store.byMessage(card.Platform, card.SessionKey, card.CardMessageID); got == nil || got.Terminal {
		t.Fatalf("active card = %#v", got)
	}

	restarted := newTurnCardStore(path)
	got := restarted.byToken(card.Token)
	if got == nil || !got.Terminal || got.Status != "stale" {
		t.Fatalf("restarted card = %#v, want stale tombstone", got)
	}
	if claimed, err := restarted.claimInterrupt(card.Token); err != nil || claimed {
		t.Fatalf("claimInterrupt() after restart = %t, %v; want false, nil", claimed, err)
	}
}

func TestTurnCardStore_ConcurrentInterruptClaimHasSingleWinner(t *testing.T) {
	store := newTurnCardStore("")
	card := testTurnCardState("token-concurrent")
	if err := store.register(card); err != nil {
		t.Fatalf("register() error = %v", err)
	}

	const workers = 12
	var wg sync.WaitGroup
	results := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := store.claimInterrupt(card.Token)
			if err != nil {
				t.Errorf("claimInterrupt() error = %v", err)
				return
			}
			results <- claimed
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for claimed := range results {
		if claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("interrupt claim winners = %d, want 1", winners)
	}
}

func TestTurnCardStore_RejectsTokenReuseForAnotherCard(t *testing.T) {
	store := newTurnCardStore("")
	first := testTurnCardState("same-token")
	if err := store.register(first); err != nil {
		t.Fatalf("first register() error = %v", err)
	}
	second := first
	second.TurnID = "turn-2"
	second.CardMessageID = "om-card-2"
	if err := store.register(second); err == nil {
		t.Fatal("token reuse register() error = nil, want rejection")
	}
}

func TestTurnCardStore_RejectsTokenReuseAcrossWorkspaceGeneration(t *testing.T) {
	store := newTurnCardStore("")
	first := testTurnCardState("workspace-token")
	if err := store.register(first); err != nil {
		t.Fatalf("first register() error = %v", err)
	}
	second := first
	second.InteractiveKey = "another-workspace:feishu:chat:user"
	if err := store.register(second); err == nil {
		t.Fatal("cross-workspace token reuse error = nil, want rejection")
	}
}
