package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStateFirstRunAnchorsNow(t *testing.T) {
	dir := t.TempDir()
	before := time.Now()
	s, err := LoadState(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if s.LastCatchupOK().Before(before) {
		t.Fatal("first run should anchor last_catchup_ok at now, not backfill history")
	}
	if _, err := os.Stat(filepath.Join(dir, "relay_state.json")); err != nil {
		t.Fatal("state file should be created on first run")
	}
}

func TestStateProcessedRoundTripAndPrune(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if s.IsProcessed(id) {
		t.Fatal("fresh id should not be processed")
	}
	s.MarkProcessed(id)
	for i := 0; i < maxProcessedIDs+50; i++ {
		s.MarkProcessed(uuid.New())
	}
	anchor := time.Now()
	s.SetLastCatchupOK(anchor)

	reloaded, err := LoadState(dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.seen) != maxProcessedIDs {
		t.Fatalf("expected processed set capped at %d, got %d", maxProcessedIDs, len(reloaded.seen))
	}
	if reloaded.IsProcessed(id) {
		t.Fatal("oldest id should have been pruned by the cap")
	}
	if !reloaded.LastCatchupOK().Equal(anchor) {
		t.Fatalf("last_catchup_ok = %v, want %v", reloaded.LastCatchupOK(), anchor)
	}
	// The anchor may move backwards when messages are still undelivered.
	reloaded.SetLastCatchupOK(anchor.Add(-time.Hour))
	if !reloaded.LastCatchupOK().Equal(anchor.Add(-time.Hour)) {
		t.Fatal("anchor should follow the caller")
	}
}

func TestStateCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay_state.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(dir, testLogger())
	if err != nil {
		t.Fatalf("corrupt state must not be fatal: %v", err)
	}
	if s.LastCatchupOK().IsZero() {
		t.Fatal("anchor should be reset to now")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatal("corrupt file should be preserved")
	}
}

func TestHealthFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteHealth(dir, HealthStatus{Connected: true}); err != nil {
		t.Fatal(err)
	}
	h, err := ReadHealth(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Connected || time.Since(h.UpdatedAt) > time.Minute {
		t.Fatalf("unexpected health %+v", h)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, healthFileName))
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
}
