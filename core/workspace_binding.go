package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const sharedWorkspaceBindingsKey = "shared"

var ErrWorkspaceBindingConflict = errors.New("workspace binding conflict")

// FlexTime wraps time.Time with lenient JSON unmarshaling.
type FlexTime struct{ time.Time }

func (ft *FlexTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		ft.Time = time.Time{}
		return nil
	}
	if s == "" {
		ft.Time = time.Time{}
		return nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			ft.Time = t
			return nil
		}
	}
	slog.Warn("workspace bindings: unparseable bound_at, treating as zero", "value", s)
	ft.Time = time.Time{}
	return nil
}

// WorkspaceBinding maps a channel to a workspace directory.
type WorkspaceBinding struct {
	ChannelName string   `json:"channel_name"`
	Workspace   string   `json:"workspace"`
	BoundAt     FlexTime `json:"bound_at"`
}

// WorkspaceBindingManager persists channel->workspace mappings.
// Top-level key is "project:<name>", second-level key is a workspace channel key.
type WorkspaceBindingManager struct {
	mu                sync.RWMutex
	bindings          map[string]map[string]*WorkspaceBinding
	storePath         string
	lastLoadedModTime time.Time
	lastLoadedSize    int64
}

func NewWorkspaceBindingManager(storePath string) *WorkspaceBindingManager {
	m := &WorkspaceBindingManager{
		bindings:  make(map[string]map[string]*WorkspaceBinding),
		storePath: storePath,
	}
	if storePath != "" {
		m.load()
	}
	return m
}

func legacyWorkspaceChannelKey(channelKey string) string {
	if i := strings.IndexByte(channelKey, ':'); i >= 0 {
		return channelKey[i+1:]
	}
	return channelKey
}

func workspaceChannelKeyCandidates(channelKey string) []string {
	if channelKey == "" {
		return nil
	}
	legacyKey := legacyWorkspaceChannelKey(channelKey)
	if legacyKey == channelKey {
		return []string{channelKey}
	}
	return []string{channelKey, legacyKey}
}

func (m *WorkspaceBindingManager) lookupLocked(projectKey, channelKey string) *WorkspaceBinding {
	proj := m.bindings[projectKey]
	if proj == nil {
		return nil
	}
	for _, candidate := range workspaceChannelKeyCandidates(channelKey) {
		if b := proj[candidate]; b != nil {
			return b
		}
	}
	return nil
}

func (m *WorkspaceBindingManager) Bind(projectKey, channelKey, channelName, workspace string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	if m.bindings[projectKey] == nil {
		m.bindings[projectKey] = make(map[string]*WorkspaceBinding)
	}
	m.bindings[projectKey][channelKey] = &WorkspaceBinding{
		ChannelName: channelName,
		Workspace:   workspace,
		BoundAt:     FlexTime{time.Now()},
	}
	m.saveLocked()
}

// BindCAS persists a binding only when it is currently absent or already
// points at workspace. It is the checked mutation used by local automation.
func (m *WorkspaceBindingManager) BindCAS(projectKey, channelKey, channelName, workspace string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	if current := m.lookupLocked(projectKey, channelKey); current != nil {
		if normalizeWorkspacePath(current.Workspace) == normalizeWorkspacePath(workspace) {
			return false, nil
		}
		return false, fmt.Errorf("%w: currently %q", ErrWorkspaceBindingConflict, current.Workspace)
	}
	if m.bindings[projectKey] == nil {
		m.bindings[projectKey] = make(map[string]*WorkspaceBinding)
	}
	m.bindings[projectKey][channelKey] = &WorkspaceBinding{ChannelName: channelName, Workspace: workspace, BoundAt: FlexTime{time.Now()}}
	if err := m.saveCheckedLocked(); err != nil {
		delete(m.bindings[projectKey], channelKey)
		if len(m.bindings[projectKey]) == 0 {
			delete(m.bindings, projectKey)
		}
		return false, err
	}
	return true, nil
}

// UnbindCAS removes only an exact project-scoped binding. The expected
// workspace guard prevents a stale closeout from deleting a newer route.
func (m *WorkspaceBindingManager) UnbindCAS(projectKey, channelKey, expectedWorkspace string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	proj := m.bindings[projectKey]
	if proj == nil {
		return false, nil
	}
	var key string
	var current *WorkspaceBinding
	for _, candidate := range workspaceChannelKeyCandidates(channelKey) {
		if b := proj[candidate]; b != nil {
			key, current = candidate, b
			break
		}
	}
	if current == nil {
		return false, nil
	}
	if normalizeWorkspacePath(current.Workspace) != normalizeWorkspacePath(expectedWorkspace) {
		return false, fmt.Errorf("%w: currently %q", ErrWorkspaceBindingConflict, current.Workspace)
	}
	delete(proj, key)
	if len(proj) == 0 {
		delete(m.bindings, projectKey)
	}
	if err := m.saveCheckedLocked(); err != nil {
		if m.bindings[projectKey] == nil {
			m.bindings[projectKey] = make(map[string]*WorkspaceBinding)
		}
		m.bindings[projectKey][key] = current
		return false, err
	}
	return true, nil
}

// LookupExact returns a copy of a project-scoped binding without falling back
// to the shared route layer.
func (m *WorkspaceBindingManager) LookupExact(projectKey, channelKey string) *WorkspaceBinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	b := m.lookupLocked(projectKey, channelKey)
	if b == nil {
		return nil
	}
	copy := *b
	return &copy
}

// MigrateChannelKey copies an existing default binding from oldChannelKey to
// newChannelKey. It never overwrites an existing destination and deliberately
// preserves the default binding for other channels and older versions.
func (m *WorkspaceBindingManager) MigrateChannelKey(projectKey, oldChannelKey, newChannelKey string) bool {
	if oldChannelKey == "" || newChannelKey == "" || oldChannelKey == newChannelKey {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()

	proj := m.bindings[projectKey]
	if proj == nil || m.lookupLocked(projectKey, newChannelKey) != nil {
		return false
	}

	var binding *WorkspaceBinding
	for _, candidate := range workspaceChannelKeyCandidates(oldChannelKey) {
		if b := proj[candidate]; b != nil {
			binding = b
			break
		}
	}
	if binding == nil {
		return false
	}

	inherited := *binding
	proj[newChannelKey] = &inherited
	m.saveLocked()
	return true
}

func (m *WorkspaceBindingManager) Unbind(projectKey, channelKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	if proj := m.bindings[projectKey]; proj != nil {
		for _, candidate := range workspaceChannelKeyCandidates(channelKey) {
			delete(proj, candidate)
		}
		if len(proj) == 0 {
			delete(m.bindings, projectKey)
		}
	}
	m.saveLocked()
}

func (m *WorkspaceBindingManager) Lookup(projectKey, channelKey string) *WorkspaceBinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	return m.lookupLocked(projectKey, channelKey)
}

// LookupEffective returns the effective binding for a channel, checking the
// current project first and then the shared routing layer.
func (m *WorkspaceBindingManager) LookupEffective(projectKey, channelKey string) (*WorkspaceBinding, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	if b := m.lookupLocked(projectKey, channelKey); b != nil {
		return b, projectKey
	}
	if b := m.lookupLocked(sharedWorkspaceBindingsKey, channelKey); b != nil {
		return b, sharedWorkspaceBindingsKey
	}
	return nil, ""
}

func (m *WorkspaceBindingManager) ListByProject(projectKey string) map[string]*WorkspaceBinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
	result := make(map[string]*WorkspaceBinding)
	if proj := m.bindings[projectKey]; proj != nil {
		for k, v := range proj {
			result[k] = v
		}
	}
	return result
}

func (m *WorkspaceBindingManager) saveLocked() {
	if err := m.saveCheckedLocked(); err != nil {
		slog.Error("workspace bindings: save error", "err", err)
	}
}

func (m *WorkspaceBindingManager) saveCheckedLocked() error {
	if m.storePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(m.bindings, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace bindings: marshal: %w", err)
	}
	if err := AtomicWriteFile(m.storePath, data, 0o644); err != nil {
		return fmt.Errorf("workspace bindings: save: %w", err)
	}
	if info, err := os.Stat(m.storePath); err == nil {
		m.lastLoadedModTime = info.ModTime()
		m.lastLoadedSize = info.Size()
	}
	return nil
}

func (m *WorkspaceBindingManager) load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshLocked()
}

func (m *WorkspaceBindingManager) refreshLocked() {
	if m.storePath == "" {
		return
	}
	info, err := os.Stat(m.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			m.bindings = make(map[string]map[string]*WorkspaceBinding)
			m.lastLoadedModTime = time.Time{}
			m.lastLoadedSize = 0
			return
		}
		slog.Error("workspace bindings: stat error", "err", err)
		return
	}
	if !m.lastLoadedModTime.IsZero() && info.ModTime().Equal(m.lastLoadedModTime) && info.Size() == m.lastLoadedSize {
		return
	}

	data, err := os.ReadFile(m.storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("workspace bindings: load error", "err", err)
		}
		return
	}
	loaded := make(map[string]map[string]*WorkspaceBinding)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &loaded); err != nil {
			slog.Error("workspace bindings: unmarshal error", "err", err)
			return
		}
	}
	m.bindings = loaded
	m.lastLoadedModTime = info.ModTime()
	m.lastLoadedSize = info.Size()
}
