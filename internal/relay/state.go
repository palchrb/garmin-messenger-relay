package relay

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/palchrb/garmin-messenger-relay/internal/atomicfile"
)

const (
	// maxProcessedIDs bounds the processed-message set kept on disk.
	maxProcessedIDs = 500
	// processedRetention is how long a processed ID is remembered. It must
	// exceed the catch-up window so catch-up never re-forwards a message.
	processedRetention = 7 * 24 * time.Hour
)

// processedEntry records that a Garmin message has been forwarded.
type processedEntry struct {
	ID uuid.UUID `json:"id"`
	At time.Time `json:"at"`
}

type stateFile struct {
	// LastCatchupOK is the wall-clock time at which the most recent catch-up
	// pass started and subsequently completed without API errors. The next
	// pass looks back from this time (minus a safety margin).
	LastCatchupOK time.Time        `json:"last_catchup_ok"`
	Processed     []processedEntry `json:"processed"`
}

// State persists the relay's processing progress: which Garmin messages have
// been forwarded and when catch-up last succeeded. It lets the relay forward
// exactly-once across restarts and reconnects.
type State struct {
	mu       sync.Mutex
	path     string
	log      *slog.Logger
	data     stateFile
	seen     map[uuid.UUID]time.Time
	firstRun bool
}

// LoadState reads the state file from sessionDir. On first run (no file)
// LastCatchupOK is set to now so that history is not backfilled. A corrupt
// file is moved aside and treated as first run.
func LoadState(sessionDir string, log *slog.Logger) (*State, error) {
	s := &State{
		path: filepath.Join(sessionDir, "relay_state.json"),
		log:  log,
		seen: make(map[uuid.UUID]time.Time),
	}
	data, err := os.ReadFile(s.path)
	switch {
	case os.IsNotExist(err):
		s.data.LastCatchupOK = time.Now()
		s.firstRun = true
		return s, s.saveLocked()
	case err != nil:
		return nil, fmt.Errorf("reading relay state: %w", err)
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		backup := s.path + ".corrupt"
		log.Error("relay state is unreadable; starting fresh", "path", s.path, "moved_to", backup, "err", err)
		if err := os.Rename(s.path, backup); err != nil {
			return nil, fmt.Errorf("moving corrupt relay state aside: %w", err)
		}
		s.data = stateFile{LastCatchupOK: time.Now()}
		s.firstRun = true
		return s, s.saveLocked()
	}
	if s.data.LastCatchupOK.IsZero() {
		s.data.LastCatchupOK = time.Now()
	}
	for _, e := range s.data.Processed {
		s.seen[e.ID] = e.At
	}
	s.pruneLocked()
	return s, nil
}

// LastCatchupOK returns the time of the last successful catch-up pass.
func (s *State) LastCatchupOK() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.LastCatchupOK
}

// FirstRun reports whether no catch-up pass has completed since the state
// file was created.
func (s *State) FirstRun() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstRun
}

// SetLastCatchupOK records the anchor for the next catch-up pass: the start
// time of a completed pass, or earlier when messages found by that pass are
// still awaiting delivery.
func (s *State) SetLastCatchupOK(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.firstRun = false
	s.data.LastCatchupOK = t
	if err := s.saveLocked(); err != nil {
		s.log.Error("failed to save relay state", "err", err)
	}
}

// IsProcessed reports whether the message has already been forwarded.
func (s *State) IsProcessed(id uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.seen[id]
	return ok
}

// MarkProcessed records that the message has been forwarded (or deliberately
// skipped) and persists the state.
func (s *State) MarkProcessed(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[id]; ok {
		return
	}
	s.seen[id] = time.Now()
	s.pruneLocked()
	if err := s.saveLocked(); err != nil {
		s.log.Error("failed to save relay state", "err", err)
	}
}

// pruneLocked drops old entries and caps the set size. Caller holds mu.
func (s *State) pruneLocked() {
	cutoff := time.Now().Add(-processedRetention)
	entries := make([]processedEntry, 0, len(s.seen))
	for id, at := range s.seen {
		if at.Before(cutoff) {
			delete(s.seen, id)
			continue
		}
		entries = append(entries, processedEntry{ID: id, At: at})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].At.After(entries[j].At) })
	if len(entries) > maxProcessedIDs {
		for _, e := range entries[maxProcessedIDs:] {
			delete(s.seen, e.ID)
		}
		entries = entries[:maxProcessedIDs]
	}
	s.data.Processed = entries
}

// saveLocked writes the state file atomically. Caller holds mu.
func (s *State) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(s.path, data, 0o600)
}

// HealthStatus is written to the session dir so the "health" command and
// Docker HEALTHCHECK can tell whether the relay is actually connected.
type HealthStatus struct {
	Connected bool      `json:"connected"`
	UpdatedAt time.Time `json:"updated_at"`
	Detail    string    `json:"detail,omitempty"`
}

// healthFileName is the health status file inside the session directory.
const healthFileName = "health.json"

// WriteHealth persists the current health status.
func WriteHealth(sessionDir string, h HealthStatus) error {
	h.UpdatedAt = time.Now()
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(sessionDir, healthFileName), data, 0o600)
}

// ReadHealth loads the health status file.
func ReadHealth(sessionDir string) (HealthStatus, error) {
	var h HealthStatus
	data, err := os.ReadFile(filepath.Join(sessionDir, healthFileName))
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return h, fmt.Errorf("parsing health file: %w", err)
	}
	return h, nil
}
