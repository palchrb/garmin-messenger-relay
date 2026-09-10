package relay

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestMsgStorePersistsPerRecipient(t *testing.T) {
	dir := t.TempDir()
	s, err := NewMsgStore(dir, 7, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	conv := uuid.New()
	for _, id := range []string{"<gmr-1@x>", "<gmr-2@x>"} {
		if err := s.Add(SentMessage{MessageID: id, ConversationID: conv, SenderPhone: "+47", RecipientEmail: id + "@r", SentAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Add(SentMessage{MessageID: "<gmr-old@x>", SentAt: time.Now().Add(-8 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewMsgStore(dir, 7, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Len() != 2 {
		t.Fatalf("expected 2 live entries after reload, got %d", reloaded.Len())
	}
	if m, ok := reloaded.Lookup("<gmr-1@x>"); !ok || m.RecipientEmail != "<gmr-1@x>@r" {
		t.Fatalf("lookup failed: %+v %v", m, ok)
	}
	if _, ok := reloaded.Lookup("<gmr-old@x>"); ok {
		t.Fatal("expired entry should not be found")
	}
	info, err := os.Stat(filepath.Join(dir, "sent_messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("store should be owner-only, got %o", info.Mode().Perm())
	}
}

func TestMsgStoreCorruptFileIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sent_messages.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewMsgStore(dir, 7, testLogger())
	if err != nil {
		t.Fatalf("corrupt store must not be fatal: %v", err)
	}
	if s.Len() != 0 {
		t.Fatal("expected empty store")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file should be preserved: %v", err)
	}
}
