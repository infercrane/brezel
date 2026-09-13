package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
)

var (
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource already exists")
)

type State struct {
	Environments map[string]domain.Environment `json:"environments"`
	Sandboxes    map[string]domain.Sandbox     `json:"sandboxes"`
	Operations   map[string]domain.Operation   `json:"operations"`
	Checkpoints  map[string]domain.Checkpoint  `json:"checkpoints"`
	Connectors   map[string]domain.Connector   `json:"connectors"`
	Events       []domain.Event                `json:"events"`
	Idempotency  map[string]string             `json:"idempotency"`
}

func NewState() State {
	return State{
		Environments: map[string]domain.Environment{},
		Sandboxes:    map[string]domain.Sandbox{},
		Operations:   map[string]domain.Operation{},
		Checkpoints:  map[string]domain.Checkpoint{},
		Connectors:   map[string]domain.Connector{},
		Events:       []domain.Event{},
		Idempotency:  map[string]string{},
	}
}

func ScopedKey(projectID, resourceID string) string {
	return projectID + "\x00" + resourceID
}

func IdempotencyKey(projectID, kind, key string) string {
	return projectID + "\x00" + kind + "\x00" + key
}

type Store interface {
	View(func(State) error) error
	Update(func(*State) error) error
}

type FileStore struct {
	mu    sync.RWMutex
	path  string
	state State
}

func OpenFile(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("state file path is required")
	}
	s := &FileStore{path: path, state: NewState()}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	s.ensureMaps()
	return s, nil
}

func (s *FileStore) View(fn func(State) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone, err := cloneState(s.state)
	if err != nil {
		return err
	}
	return fn(clone)
}

func (s *FileStore) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := cloneState(s.state)
	if err != nil {
		return err
	}
	if err := fn(&next); err != nil {
		return err
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *FileStore) persist(next State) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".runtime-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func (s *FileStore) ensureMaps() {
	if s.state.Environments == nil {
		s.state.Environments = map[string]domain.Environment{}
	}
	if s.state.Sandboxes == nil {
		s.state.Sandboxes = map[string]domain.Sandbox{}
	}
	if s.state.Operations == nil {
		s.state.Operations = map[string]domain.Operation{}
	}
	if s.state.Checkpoints == nil {
		s.state.Checkpoints = map[string]domain.Checkpoint{}
	}
	if s.state.Connectors == nil {
		s.state.Connectors = map[string]domain.Connector{}
	}
	if s.state.Events == nil {
		s.state.Events = []domain.Event{}
	}
	if s.state.Idempotency == nil {
		s.state.Idempotency = map[string]string{}
	}
}

func cloneState(in State) (State, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return State{}, err
	}
	out := NewState()
	if err := json.Unmarshal(data, &out); err != nil {
		return State{}, err
	}
	return out, nil
}
