package core

import (
	"crypto/sha256"
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
	trackStateVersion      = 1
	trackReservationMaxAge = 24 * time.Hour
	trackRecentTurnLimit   = 64
)

type trackOverride string

const (
	trackOverrideUnset trackOverride = ""
	trackOverrideOn    trackOverride = "on"
	trackOverrideOff   trackOverride = "off"
)

type trackBindingState struct {
	Destination   string        `json:"destination"`
	SessionKey    string        `json:"session_key"`
	Platform      string        `json:"platform"`
	ThreadID      string        `json:"thread_id,omitempty"`
	Override      trackOverride `json:"override,omitempty"`
	Generation    uint64        `json:"generation"`
	Initialized   bool          `json:"initialized,omitempty"`
	Watermark     string        `json:"watermark,omitempty"`
	RecentTurnIDs []string      `json:"recent_turn_ids,omitempty"`
	LastTurnID    string        `json:"last_turn_id,omitempty"`
	Gap           string        `json:"gap,omitempty"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

type trackDeliveryState struct {
	Key               string    `json:"key"`
	Destination       string    `json:"destination"`
	SessionKey        string    `json:"session_key"`
	Platform          string    `json:"platform"`
	ThreadID          string    `json:"thread_id"`
	TurnID            string    `json:"turn_id"`
	Generation        uint64    `json:"generation"`
	Purpose           string    `json:"purpose"`
	Source            string    `json:"source"`
	ClientID          string    `json:"client_id,omitempty"`
	CardCreateKey     string    `json:"card_create_key"`
	CardHandle        string    `json:"card_handle,omitempty"`
	CardMessageID     string    `json:"card_message_id,omitempty"`
	RenderHash        string    `json:"render_hash,omitempty"`
	Status            string    `json:"status,omitempty"`
	Terminal          bool      `json:"terminal,omitempty"`
	NotificationState string    `json:"notification_state,omitempty"`
	NotificationKey   string    `json:"notification_key"`
	LastError         string    `json:"last_error,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type trackForegroundReservation struct {
	ClientID     string    `json:"client_id"`
	Destination  string    `json:"destination"`
	SessionKey   string    `json:"session_key"`
	ThreadID     string    `json:"thread_id"`
	TurnID       string    `json:"turn_id,omitempty"`
	Generation   uint64    `json:"generation"`
	SourceMsgID  string    `json:"source_message_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	LastObserved time.Time `json:"last_observed,omitempty"`
}

type trackPersistedState struct {
	Version      int                                    `json:"version"`
	Bindings     map[string]*trackBindingState          `json:"bindings"`
	Deliveries   map[string]*trackDeliveryState         `json:"deliveries"`
	Reservations map[string]*trackForegroundReservation `json:"reservations"`
}

type trackStateStore struct {
	mu    sync.Mutex
	path  string
	state trackPersistedState
}

func trackStatePath(sessionStorePath string) string {
	sessionStorePath = strings.TrimSpace(sessionStorePath)
	if sessionStorePath == "" {
		return ""
	}
	return sessionStorePath + ".track.json"
}

func newTrackStateStore(path string) *trackStateStore {
	s := &trackStateStore{path: strings.TrimSpace(path)}
	s.state = trackPersistedState{
		Version:      trackStateVersion,
		Bindings:     make(map[string]*trackBindingState),
		Deliveries:   make(map[string]*trackDeliveryState),
		Reservations: make(map[string]*trackForegroundReservation),
	}
	s.load()
	return s
}

func (s *trackStateStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("track: read state failed", "path", s.path, "error", err)
		}
		return
	}
	var state trackPersistedState
	if err := json.Unmarshal(b, &state); err != nil {
		slog.Warn("track: parse state failed", "path", s.path, "error", err)
		return
	}
	if state.Bindings == nil {
		state.Bindings = make(map[string]*trackBindingState)
	}
	if state.Deliveries == nil {
		state.Deliveries = make(map[string]*trackDeliveryState)
	}
	if state.Reservations == nil {
		state.Reservations = make(map[string]*trackForegroundReservation)
	}
	state.Version = trackStateVersion
	s.state = state
	s.pruneLocked(time.Now())
}

func (s *trackStateStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	s.pruneLocked(time.Now())
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("track: marshal state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("track: create state directory: %w", err)
	}
	if err := AtomicWriteFile(s.path, b, 0o600); err != nil {
		return fmt.Errorf("track: write state: %w", err)
	}
	return nil
}

func (s *trackStateStore) pruneLocked(now time.Time) {
	for key, reservation := range s.state.Reservations {
		if reservation == nil || (!reservation.CreatedAt.IsZero() && now.Sub(reservation.CreatedAt) > trackReservationMaxAge) {
			delete(s.state.Reservations, key)
		}
	}
	// Retain all active deliveries and a bounded terminal tail per destination.
	byDestination := make(map[string][]*trackDeliveryState)
	for key, delivery := range s.state.Deliveries {
		if delivery == nil {
			delete(s.state.Deliveries, key)
			continue
		}
		if delivery.Terminal {
			byDestination[delivery.Destination] = append(byDestination[delivery.Destination], delivery)
		}
	}
	for _, deliveries := range byDestination {
		if len(deliveries) <= trackRecentTurnLimit {
			continue
		}
		sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].UpdatedAt.After(deliveries[j].UpdatedAt) })
		for _, delivery := range deliveries[trackRecentTurnLimit:] {
			delete(s.state.Deliveries, delivery.Key)
		}
	}
}

func cloneTrackBinding(binding *trackBindingState) *trackBindingState {
	if binding == nil {
		return nil
	}
	clone := *binding
	clone.RecentTurnIDs = append([]string(nil), binding.RecentTurnIDs...)
	return &clone
}

func cloneTrackDelivery(delivery *trackDeliveryState) *trackDeliveryState {
	if delivery == nil {
		return nil
	}
	clone := *delivery
	return &clone
}

func cloneTrackReservation(reservation *trackForegroundReservation) *trackForegroundReservation {
	if reservation == nil {
		return nil
	}
	clone := *reservation
	return &clone
}

func cloneTrackPersistedState(state trackPersistedState) trackPersistedState {
	clone := trackPersistedState{
		Version:      state.Version,
		Bindings:     make(map[string]*trackBindingState, len(state.Bindings)),
		Deliveries:   make(map[string]*trackDeliveryState, len(state.Deliveries)),
		Reservations: make(map[string]*trackForegroundReservation, len(state.Reservations)),
	}
	for key, binding := range state.Bindings {
		clone.Bindings[key] = cloneTrackBinding(binding)
	}
	for key, delivery := range state.Deliveries {
		clone.Deliveries[key] = cloneTrackDelivery(delivery)
	}
	for key, reservation := range state.Reservations {
		clone.Reservations[key] = cloneTrackReservation(reservation)
	}
	return clone
}

func (s *trackStateStore) commitLocked(previous trackPersistedState) error {
	if err := s.saveLocked(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func (s *trackStateStore) binding(destination string) *trackBindingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTrackBinding(s.state.Bindings[destination])
}

func (s *trackStateStore) bindings() []*trackBindingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	bindings := make([]*trackBindingState, 0, len(s.state.Bindings))
	for _, binding := range s.state.Bindings {
		bindings = append(bindings, cloneTrackBinding(binding))
	}
	return bindings
}

func (s *trackStateStore) bind(destination, sessionKey, platform, threadID string) (*trackBindingState, error) {
	destination = strings.TrimSpace(destination)
	sessionKey = strings.TrimSpace(sessionKey)
	threadID = strings.TrimSpace(threadID)
	if destination == "" || sessionKey == "" || threadID == "" {
		return nil, fmt.Errorf("track: destination, session key, and thread id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for otherDestination, other := range s.state.Bindings {
		if otherDestination != destination && other != nil && other.ThreadID == threadID {
			return nil, fmt.Errorf("track: thread is already bound to another destination")
		}
	}
	previous := cloneTrackPersistedState(s.state)
	now := time.Now()
	binding := s.state.Bindings[destination]
	if binding == nil {
		binding = &trackBindingState{Destination: destination, Generation: 1}
		s.state.Bindings[destination] = binding
	} else if binding.ThreadID != "" && binding.ThreadID != threadID {
		if binding.SessionKey != "" && binding.SessionKey != sessionKey {
			return nil, fmt.Errorf("track: destination is already bound to another logical session")
		}
		binding.Generation++
		binding.Initialized = false
		binding.Watermark = ""
		binding.RecentTurnIDs = nil
		binding.LastTurnID = ""
		binding.Gap = ""
	}
	if binding.Generation == 0 {
		binding.Generation = 1
	}
	binding.SessionKey = sessionKey
	binding.Platform = strings.TrimSpace(platform)
	binding.ThreadID = threadID
	binding.UpdatedAt = now
	if err := s.commitLocked(previous); err != nil {
		return nil, err
	}
	return cloneTrackBinding(binding), nil
}

func (s *trackStateStore) unbind(destination, sessionKey, platform string) (*trackBindingState, error) {
	destination = strings.TrimSpace(destination)
	sessionKey = strings.TrimSpace(sessionKey)
	if destination == "" {
		return nil, fmt.Errorf("track: destination is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := s.state.Bindings[destination]
	if binding == nil {
		return nil, nil
	}
	if binding.SessionKey != "" && sessionKey != "" && binding.SessionKey != sessionKey {
		return cloneTrackBinding(binding), nil
	}
	previous := cloneTrackPersistedState(s.state)
	binding.Generation++
	if binding.Generation == 0 {
		binding.Generation = 1
	}
	binding.SessionKey = sessionKey
	binding.Platform = strings.TrimSpace(platform)
	binding.ThreadID = ""
	binding.Initialized = false
	binding.Watermark = ""
	binding.RecentTurnIDs = nil
	binding.LastTurnID = ""
	binding.Gap = ""
	binding.UpdatedAt = time.Now()
	if err := s.commitLocked(previous); err != nil {
		return nil, err
	}
	return cloneTrackBinding(binding), nil
}

func (s *trackStateStore) setOverride(destination, sessionKey, platform string, override trackOverride) (*trackBindingState, error) {
	if override != trackOverrideUnset && override != trackOverrideOn && override != trackOverrideOff {
		return nil, fmt.Errorf("track: invalid override %q", override)
	}
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return nil, fmt.Errorf("track: destination is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneTrackPersistedState(s.state)
	binding := s.state.Bindings[destination]
	if binding == nil {
		binding = &trackBindingState{Destination: destination, Generation: 1}
		s.state.Bindings[destination] = binding
	}
	binding.SessionKey = strings.TrimSpace(sessionKey)
	binding.Platform = strings.TrimSpace(platform)
	binding.Override = override
	binding.UpdatedAt = time.Now()
	if err := s.commitLocked(previous); err != nil {
		return nil, err
	}
	return cloneTrackBinding(binding), nil
}

func (s *trackStateStore) setInitialized(destination, watermark string, recent []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := s.state.Bindings[destination]
	if binding == nil {
		return fmt.Errorf("track: destination binding not found")
	}
	previous := cloneTrackPersistedState(s.state)
	binding.Initialized = true
	binding.Watermark = strings.TrimSpace(watermark)
	binding.RecentTurnIDs = boundedRecentTurnIDs(recent)
	binding.Gap = ""
	binding.UpdatedAt = time.Now()
	return s.commitLocked(previous)
}

func (s *trackStateStore) resetBaseline(destination string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := s.state.Bindings[destination]
	if binding == nil {
		return fmt.Errorf("track: destination binding not found")
	}
	previous := cloneTrackPersistedState(s.state)
	binding.Initialized = false
	binding.Watermark = ""
	binding.RecentTurnIDs = nil
	binding.Gap = ""
	binding.UpdatedAt = time.Now()
	return s.commitLocked(previous)
}

func (s *trackStateStore) setGap(destination, gap string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := s.state.Bindings[destination]
	if binding == nil {
		return fmt.Errorf("track: destination binding not found")
	}
	previous := cloneTrackPersistedState(s.state)
	binding.Gap = strings.TrimSpace(gap)
	binding.UpdatedAt = time.Now()
	return s.commitLocked(previous)
}

func (s *trackStateStore) clearGap(destination string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := s.state.Bindings[destination]
	if binding == nil {
		return fmt.Errorf("track: destination binding not found")
	}
	if binding.Gap == "" {
		return nil
	}
	previous := cloneTrackPersistedState(s.state)
	binding.Gap = ""
	binding.UpdatedAt = time.Now()
	return s.commitLocked(previous)
}

func boundedRecentTurnIDs(ids []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, min(len(ids), trackRecentTurnLimit))
	for i := len(ids) - 1; i >= 0 && len(result) < trackRecentTurnLimit; i-- {
		id := strings.TrimSpace(ids[i])
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

func (s *trackStateStore) markTurnObserved(destination, turnID string) error {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := s.state.Bindings[destination]
	if binding == nil {
		return fmt.Errorf("track: destination binding not found")
	}
	previous := cloneTrackPersistedState(s.state)
	binding.LastTurnID = turnID
	binding.Watermark = turnID
	binding.RecentTurnIDs = boundedRecentTurnIDs(append(binding.RecentTurnIDs, turnID))
	binding.UpdatedAt = time.Now()
	return s.commitLocked(previous)
}

func (s *trackStateStore) reserveForeground(reservation trackForegroundReservation) error {
	reservation.ClientID = strings.TrimSpace(reservation.ClientID)
	if reservation.ClientID == "" {
		return nil
	}
	reservation.CreatedAt = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneTrackPersistedState(s.state)
	s.state.Reservations[reservation.ClientID] = cloneTrackReservation(&reservation)
	return s.commitLocked(previous)
}

func (s *trackStateStore) foregroundReservation(clientID string) *trackForegroundReservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTrackReservation(s.state.Reservations[strings.TrimSpace(clientID)])
}

func (s *trackStateStore) foregroundReservationForTurn(threadID, turnID string) *trackForegroundReservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, reservation := range s.state.Reservations {
		if reservation != nil && reservation.ThreadID == threadID && reservation.TurnID == turnID && turnID != "" {
			return cloneTrackReservation(reservation)
		}
	}
	return nil
}

func (s *trackStateStore) confirmForegroundTurn(clientID, threadID, turnID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	reservation := s.state.Reservations[strings.TrimSpace(clientID)]
	if reservation == nil {
		return nil
	}
	if reservation.ThreadID != "" && reservation.ThreadID != strings.TrimSpace(threadID) {
		return fmt.Errorf("track: foreground marker resolved to another thread")
	}
	previous := cloneTrackPersistedState(s.state)
	reservation.ThreadID = strings.TrimSpace(threadID)
	reservation.TurnID = strings.TrimSpace(turnID)
	reservation.LastObserved = time.Now()
	return s.commitLocked(previous)
}

func trackDeliveryKey(destination, threadID, turnID, purpose string) string {
	hash := sha256.Sum256([]byte(strings.Join([]string{destination, threadID, turnID, purpose}, "\x00")))
	return hex.EncodeToString(hash[:])
}

func trackIdempotencyKey(deliveryKey, suffix string) string {
	hash := sha256.Sum256([]byte(deliveryKey + "\x00" + suffix))
	return hex.EncodeToString(hash[:16])
}

func trackRenderHash(payload string) string {
	hash := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(hash[:])
}

func (s *trackStateStore) delivery(destination, threadID, turnID, purpose string) *trackDeliveryState {
	key := trackDeliveryKey(destination, threadID, turnID, purpose)
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTrackDelivery(s.state.Deliveries[key])
}

func (s *trackStateStore) deliveryByKey(key string) *trackDeliveryState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneTrackDelivery(s.state.Deliveries[strings.TrimSpace(key)])
}

func (s *trackStateStore) deliveryByCardMessageID(destination, messageID string) *trackDeliveryState {
	destination = strings.TrimSpace(destination)
	messageID = strings.TrimSpace(messageID)
	if destination == "" || messageID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var matched *trackDeliveryState
	for _, delivery := range s.state.Deliveries {
		if delivery == nil || delivery.Destination != destination || delivery.CardMessageID != messageID {
			continue
		}
		if matched == nil || delivery.UpdatedAt.After(matched.UpdatedAt) {
			matched = delivery
		}
	}
	return cloneTrackDelivery(matched)
}

func (s *trackStateStore) claimDelivery(binding *trackBindingState, turnID, purpose, source, clientID string) (*trackDeliveryState, bool, error) {
	if binding == nil || binding.Destination == "" || binding.ThreadID == "" || strings.TrimSpace(turnID) == "" {
		return nil, false, fmt.Errorf("track: incomplete delivery identity")
	}
	if purpose == "" {
		purpose = "primary"
	}
	key := trackDeliveryKey(binding.Destination, binding.ThreadID, turnID, purpose)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.state.Deliveries[key]; existing != nil {
		return cloneTrackDelivery(existing), false, nil
	}
	previous := cloneTrackPersistedState(s.state)
	now := time.Now()
	delivery := &trackDeliveryState{
		Key: key, Destination: binding.Destination, SessionKey: binding.SessionKey,
		Platform: binding.Platform, ThreadID: binding.ThreadID, TurnID: strings.TrimSpace(turnID),
		Generation: binding.Generation, Purpose: purpose, Source: source, ClientID: strings.TrimSpace(clientID),
		CardCreateKey: trackIdempotencyKey(key, "card"), NotificationKey: trackIdempotencyKey(key, "terminal"),
		NotificationState: "none", CreatedAt: now, UpdatedAt: now,
	}
	s.state.Deliveries[key] = delivery
	if err := s.commitLocked(previous); err != nil {
		return nil, false, err
	}
	return cloneTrackDelivery(delivery), true, nil
}

func (s *trackStateStore) updateDelivery(key string, mutate func(*trackDeliveryState)) (*trackDeliveryState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delivery := s.state.Deliveries[key]
	if delivery == nil {
		return nil, fmt.Errorf("track: delivery not found")
	}
	previous := cloneTrackPersistedState(s.state)
	mutate(delivery)
	delivery.UpdatedAt = time.Now()
	if err := s.commitLocked(previous); err != nil {
		return nil, err
	}
	return cloneTrackDelivery(delivery), nil
}

func (s *trackStateStore) setDeliveryHandle(key, handle, messageID string) (*trackDeliveryState, error) {
	return s.updateDelivery(key, func(delivery *trackDeliveryState) {
		delivery.CardHandle = handle
		if strings.TrimSpace(messageID) != "" {
			delivery.CardMessageID = strings.TrimSpace(messageID)
		}
		delivery.LastError = ""
	})
}

func (s *trackStateStore) setDeliveryRender(key, renderHash, status string, terminal bool) (*trackDeliveryState, error) {
	return s.updateDelivery(key, func(delivery *trackDeliveryState) {
		if delivery.Terminal && !terminal {
			return
		}
		delivery.RenderHash = renderHash
		delivery.Status = status
		delivery.Terminal = terminal
		delivery.LastError = ""
		if terminal && delivery.NotificationState == "none" {
			delivery.NotificationState = "pending"
		}
	})
}

func (s *trackStateStore) setDeliveryError(key, message string) error {
	_, err := s.updateDelivery(key, func(delivery *trackDeliveryState) {
		delivery.LastError = strings.TrimSpace(message)
	})
	return err
}

func (s *trackStateStore) markNotificationSent(key string) error {
	_, err := s.updateDelivery(key, func(delivery *trackDeliveryState) {
		delivery.NotificationState = "sent"
	})
	return err
}

func (s *trackStateStore) activeDeliveries(destination string) []*trackDeliveryState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deliveries []*trackDeliveryState
	for _, delivery := range s.state.Deliveries {
		if delivery != nil && delivery.Destination == destination && !delivery.Terminal && delivery.Purpose == "primary" {
			deliveries = append(deliveries, cloneTrackDelivery(delivery))
		}
	}
	sort.Slice(deliveries, func(i, j int) bool { return deliveries[i].UpdatedAt.After(deliveries[j].UpdatedAt) })
	return deliveries
}
