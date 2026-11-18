package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Settings are the user-editable options that shape the generated sing-box config.
type Settings struct {
	ListenAddr  string `json:"listenAddr"`
	ListenPort  int    `json:"listenPort"`
	ProxyUser   string `json:"proxyUser"`
	ProxyPass   string `json:"proxyPass"`
	DNSServer   string `json:"dnsServer"`
	DNSStrategy string `json:"dnsStrategy"`
	TestURL     string `json:"testUrl"`
	LogLevel    string `json:"logLevel"`
	BypassChina bool   `json:"bypassChina"`
}

func defaultSettings() Settings {
	return Settings{
		ListenAddr: "127.0.0.1",
		ListenPort: 7890,
		DNSServer:  "local",
		TestURL:    "https://www.gstatic.com/generate_204",
		LogLevel:   "info",
	}
}

// Node is one outbound imported from a subscription, kept verbatim in Raw.
type Node struct {
	Tag    string          `json:"tag"`
	Type   string          `json:"type"`
	Server string          `json:"server,omitempty"`
	Port   int             `json:"port,omitempty"`
	Helper bool            `json:"helper,omitempty"` // only used as another node's detour
	Raw    json.RawMessage `json:"raw"`
}

// How a subscription is downloaded. "auto" goes through the running proxy so the
// request (and the secret in its URL) never crosses the firewall in clear text, and
// falls back to a direct connection when the proxy is down or fails.
const (
	updateViaAuto   = "auto"
	updateViaProxy  = "proxy"
	updateViaDirect = "direct"
)

type Subscription struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	URL           string    `json:"url"`
	UserAgent     string    `json:"userAgent,omitempty"`
	IntervalHours int       `json:"intervalHours"` // 0 = manual updates only
	UpdateVia     string    `json:"updateVia"`
	ViaProxy      bool      `json:"viaProxy,omitempty"` // legacy, migrated to UpdateVia
	UpdatedVia    string    `json:"updatedVia,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
	AttemptedAt   time.Time `json:"attemptedAt"`
	LastError     string    `json:"lastError,omitempty"`
	Nodes         []Node    `json:"nodes"`
}

// NodeRef identifies a node independently of the tag it gets in the generated config.
type NodeRef struct {
	SubID string `json:"subId"`
	Tag   string `json:"tag"`
}

type State struct {
	Settings      Settings        `json:"settings"`
	Subscriptions []*Subscription `json:"subscriptions"`
	Selected      *NodeRef        `json:"selected,omitempty"`
	CoreEnabled   bool            `json:"coreEnabled"`
	ClashPort     int             `json:"clashPort"`
	ClashSecret   string          `json:"clashSecret"`

	RuleSetsUpdatedAt time.Time `json:"ruleSetsUpdatedAt"`
}

func (s *State) findSubscription(id string) *Subscription {
	for _, sub := range s.Subscriptions {
		if sub.ID == id {
			return sub
		}
	}
	return nil
}

// Store keeps the whole state in memory and persists it as one JSON file.
type Store struct {
	path  string
	mu    sync.Mutex
	state *State
}

func OpenStore(path string) (*Store, error) {
	store := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		store.state = &State{Settings: defaultSettings()}
		return store, nil
	case err != nil:
		return nil, err
	}
	state := &State{Settings: defaultSettings()}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	migrateState(state)
	store.state = state
	return store, nil
}

func migrateState(state *State) {
	for _, sub := range state.Subscriptions {
		if sub.UpdateVia == "" {
			sub.UpdateVia = updateViaAuto
			if sub.ViaProxy {
				sub.UpdateVia = updateViaProxy
			}
		}
		sub.ViaProxy = false
	}
}

// Snapshot returns a deep copy that callers may read without holding the lock.
func (s *Store) Snapshot() *State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state)
}

// Update applies fn to a copy and only commits it if fn succeeds and the file is written.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	if err := fn(next); err != nil {
		return err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, data, 0o600); err != nil {
		return err
	}
	s.state = next
	return nil
}

func cloneState(state *State) *State {
	data, err := json.Marshal(state)
	if err != nil {
		panic(err)
	}
	clone := &State{}
	if err := json.Unmarshal(data, clone); err != nil {
		panic(err)
	}
	return clone
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(perm); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
