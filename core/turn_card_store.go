package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	turnCardStateVersion = 1
	turnCardRetention    = 30 * 24 * time.Hour
	turnCardMaxEntries   = 512
)

// turnCardState contains only the identity needed to make native task-card
// actions fail closed. Conversation content and authoritative lifecycle state
// remain owned by the agent backend.
type turnCardState struct {
	Token              string    `json:"token"`
	Platform           string    `json:"platform"`
	SessionKey         string    `json:"session_key"`
	Destination        string    `json:"destination,omitempty"`
	InteractiveKey     string    `json:"interactive_key"`
	ThreadID           string    `json:"thread_id"`
	TurnID             string    `json:"turn_id"`
	Generation         uint64    `json:"generation"`
	CardMessageID      string    `json:"card_message_id"`
	Status             string    `json:"status,omitempty"`
	Terminal           bool      `json:"terminal,omitempty"`
	InterruptRequested bool      `json:"interrupt_requested,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type turnCardPersistedState struct {
	Version int                       `json:"version"`
	Cards   map[string]*turnCardState `json:"cards"`
}

type turnCardStore struct {
	mu    sync.Mutex
	path  string
	state turnCardPersistedState
}

func turnCardStatePath(sessionStorePath string) string {
	sessionStorePath = strings.TrimSpace(sessionStorePath)
	if sessionStorePath == "" {
		return ""
	}
	return sessionStorePath + ".turn-cards.json"
}

func newTurnCardStore(path string) *turnCardStore {
	s := &turnCardStore{
		path: strings.TrimSpace(path),
		state: turnCardPersistedState{
			Version: turnCardStateVersion,
			Cards:   make(map[string]*turnCardState),
		},
	}
	s.load()
	return s
}

func (s *turnCardStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("turn card: read state failed", "path", s.path, "error", err)
		}
		return
	}
	var state turnCardPersistedState
	if err := json.Unmarshal(b, &state); err != nil {
		slog.Warn("turn card: parse state failed", "path", s.path, "error", err)
		return
	}
	if state.Cards == nil {
		state.Cards = make(map[string]*turnCardState)
	}
	state.Version = turnCardStateVersion
	s.state = state

	// A process restart deliberately does not recover control ownership for an
	// old native card. Keep the identity as a stale tombstone so replies and
	// button callbacks cannot accidentally target a newer turn.
	now := time.Now()
	changed := false
	for _, card := range s.state.Cards {
		if card != nil && !card.Terminal {
			card.Terminal = true
			card.Status = "stale"
			card.UpdatedAt = now
			changed = true
		}
	}
	if s.pruneLocked(now) {
		changed = true
	}
	if changed {
		if err := s.saveLocked(); err != nil {
			slog.Warn("turn card: persist restart tombstones failed", "path", s.path, "error", err)
		}
	}
}

func cloneTurnCardState(card *turnCardState) *turnCardState {
	if card == nil {
		return nil
	}
	copy := *card
	return &copy
}

func (s *turnCardStore) snapshotLocked() turnCardPersistedState {
	snapshot := turnCardPersistedState{
		Version: turnCardStateVersion,
		Cards:   make(map[string]*turnCardState, len(s.state.Cards)),
	}
	for token, card := range s.state.Cards {
		snapshot.Cards[token] = cloneTurnCardState(card)
	}
	return snapshot
}

func (s *turnCardStore) restoreLocked(snapshot turnCardPersistedState) {
	s.state = snapshot
}

func (s *turnCardStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("turn card: marshal state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("turn card: create state directory: %w", err)
	}
	if err := AtomicWriteFile(s.path, b, 0o600); err != nil {
		return fmt.Errorf("turn card: write state: %w", err)
	}
	return nil
}

func (s *turnCardStore) commitLocked(previous turnCardPersistedState) error {
	s.pruneLocked(time.Now())
	if err := s.saveLocked(); err != nil {
		s.restoreLocked(previous)
		return err
	}
	return nil
}

func (s *turnCardStore) pruneLocked(now time.Time) bool {
	changed := false
	for token, card := range s.state.Cards {
		if card == nil || strings.TrimSpace(card.Token) == "" ||
			(!card.UpdatedAt.IsZero() && now.Sub(card.UpdatedAt) > turnCardRetention) {
			delete(s.state.Cards, token)
			changed = true
		}
	}
	if len(s.state.Cards) <= turnCardMaxEntries {
		return changed
	}
	type cardAge struct {
		token string
		at    time.Time
	}
	terminal := make([]cardAge, 0, len(s.state.Cards))
	for token, card := range s.state.Cards {
		if card != nil && card.Terminal {
			terminal = append(terminal, cardAge{token: token, at: card.UpdatedAt})
		}
	}
	sort.Slice(terminal, func(i, j int) bool { return terminal[i].at.Before(terminal[j].at) })
	for _, entry := range terminal {
		if len(s.state.Cards) <= turnCardMaxEntries {
			break
		}
		delete(s.state.Cards, entry.token)
		changed = true
	}
	return changed
}

func newTurnCardToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("turn card: generate action token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func validateTurnCardIdentity(card *turnCardState) error {
	if card == nil || strings.TrimSpace(card.Token) == "" || strings.TrimSpace(card.Platform) == "" ||
		strings.TrimSpace(card.SessionKey) == "" || strings.TrimSpace(card.InteractiveKey) == "" ||
		strings.TrimSpace(card.ThreadID) == "" || strings.TrimSpace(card.TurnID) == "" ||
		card.Generation == 0 || strings.TrimSpace(card.CardMessageID) == "" {
		return fmt.Errorf("turn card: incomplete card identity")
	}
	return nil
}

func (s *turnCardStore) register(card turnCardState) error {
	if err := validateTurnCardIdentity(&card); err != nil {
		return err
	}
	now := time.Now()
	card.CreatedAt = now
	card.UpdatedAt = now
	card.Terminal = false
	card.InterruptRequested = false

	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.snapshotLocked()
	if existing := s.state.Cards[card.Token]; existing != nil {
		if existing.Platform != card.Platform || existing.SessionKey != card.SessionKey ||
			existing.InteractiveKey != card.InteractiveKey ||
			existing.ThreadID != card.ThreadID || existing.TurnID != card.TurnID ||
			existing.Generation != card.Generation || existing.CardMessageID != card.CardMessageID {
			return fmt.Errorf("turn card: action token already belongs to another card")
		}
		return nil
	}
	s.state.Cards[card.Token] = cloneTurnCardState(&card)
	return s.commitLocked(previous)
}

func (s *turnCardStore) byToken(token string) *turnCardState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTurnCardState(s.state.Cards[strings.TrimSpace(token)])
}

func (s *turnCardStore) byMessage(platform, sessionKey, messageID string) *turnCardState {
	platform = strings.TrimSpace(platform)
	sessionKey = strings.TrimSpace(sessionKey)
	messageID = strings.TrimSpace(messageID)
	if platform == "" || messageID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched *turnCardState
	for _, card := range s.state.Cards {
		if card == nil || !strings.EqualFold(card.Platform, platform) ||
			(sessionKey != "" && card.SessionKey != sessionKey) || card.CardMessageID != messageID {
			continue
		}
		if matched == nil || card.UpdatedAt.After(matched.UpdatedAt) {
			matched = card
		}
	}
	return cloneTurnCardState(matched)
}

func (s *turnCardStore) byTurn(platform, sessionKey, destination, threadID, turnID string) *turnCardState {
	platform = strings.TrimSpace(platform)
	sessionKey = strings.TrimSpace(sessionKey)
	destination = strings.TrimSpace(destination)
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if platform == "" || threadID == "" || turnID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched *turnCardState
	for _, card := range s.state.Cards {
		if card == nil || !strings.EqualFold(card.Platform, platform) || card.ThreadID != threadID || card.TurnID != turnID {
			continue
		}
		sameDestination := destination != "" && card.Destination != "" && card.Destination == destination
		if card.SessionKey != sessionKey && !sameDestination {
			continue
		}
		if matched == nil || card.UpdatedAt.After(matched.UpdatedAt) {
			matched = card
		}
	}
	return cloneTurnCardState(matched)
}

func (s *turnCardStore) markTerminal(token, status string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	card := s.state.Cards[token]
	if card == nil {
		return nil
	}
	previous := s.snapshotLocked()
	card.Terminal = true
	card.Status = strings.TrimSpace(status)
	card.UpdatedAt = time.Now()
	return s.commitLocked(previous)
}

func (s *turnCardStore) claimInterrupt(token string) (bool, error) {
	token = strings.TrimSpace(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	card := s.state.Cards[token]
	if card == nil || card.Terminal || card.InterruptRequested {
		return false, nil
	}
	previous := s.snapshotLocked()
	card.InterruptRequested = true
	card.UpdatedAt = time.Now()
	if err := s.commitLocked(previous); err != nil {
		return false, err
	}
	return true, nil
}

func (s *turnCardStore) releaseInterrupt(token string) error {
	token = strings.TrimSpace(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	card := s.state.Cards[token]
	if card == nil || card.Terminal || !card.InterruptRequested {
		return nil
	}
	previous := s.snapshotLocked()
	card.InterruptRequested = false
	card.UpdatedAt = time.Now()
	return s.commitLocked(previous)
}
