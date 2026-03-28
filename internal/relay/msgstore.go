package relay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SentMessage records the metadata of an email sent on behalf of a Garmin message.
// It is used to match inbound replies back to the originating Garmin conversation.
type SentMessage struct {
	// MessageID is the RFC 5322 Message-ID header value of the sent email,
	// e.g. "<gmr-CONVID-MSGID-RAND@garmin-messenger-relay.local>".
	MessageID string `json:"message_id"`
	// ConversationID is the Garmin Messenger conversation UUID.
	ConversationID uuid.UUID `json:"conversation_id"`
	// SenderPhone is the E.164 phone number of the Garmin sender.
	// Used as the recipient when sending the reply back.
	SenderPhone string `json:"sender_phone"`
	// RecipientEmail is the email address the message was forwarded to.
	// Replies from any other address are rejected.
	RecipientEmail string `json:"recipient_email"`
	// SentAt is when the email was dispatched.
	SentAt time.Time `json:"sent_at"`
}

// MsgStore persists sent-message metadata so replies can be matched to Garmin
// conversations across restarts.
type MsgStore struct {
	mu       sync.RWMutex
	path     string
	messages map[string]SentMessage // keyed by MessageID
	windowDays int
}

// NewMsgStore creates or loads a store from path.
// windowDays entries older than that are pruned on load and lookup.
func NewMsgStore(sessionDir string, windowDays int) (*MsgStore, error) {
	path := filepath.Join(sessionDir, "sent_messages.json")
	s := &MsgStore{
		path:       path,
		messages:   make(map[string]SentMessage),
		windowDays: windowDays,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Add records a newly sent email. It is safe to call concurrently.
func (s *MsgStore) Add(m SentMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages[m.MessageID] = m
	return s.save()
}

// Lookup returns the SentMessage for a given email Message-ID and true,
// or false if not found or expired.
func (s *MsgStore) Lookup(messageID string) (SentMessage, bool) {
	s.mu.RLock()
	m, ok := s.messages[messageID]
	s.mu.RUnlock()
	if !ok {
		return SentMessage{}, false
	}
	if s.expired(m) {
		return SentMessage{}, false
	}
	return m, true
}

func (s *MsgStore) expired(m SentMessage) bool {
	return time.Since(m.SentAt) > time.Duration(s.windowDays)*24*time.Hour
}

func (s *MsgStore) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading msgstore: %w", err)
	}
	var messages []SentMessage
	if err := json.Unmarshal(data, &messages); err != nil {
		return fmt.Errorf("parsing msgstore: %w", err)
	}
	cutoff := time.Now().Add(-time.Duration(s.windowDays) * 24 * time.Hour)
	for _, m := range messages {
		if m.SentAt.After(cutoff) {
			s.messages[m.MessageID] = m
		}
	}
	return nil
}

func (s *MsgStore) save() error {
	messages := make([]SentMessage, 0, len(s.messages))
	for _, m := range s.messages {
		if !s.expired(m) {
			messages = append(messages, m)
		}
	}
	data, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		return fmt.Errorf("serialising msgstore: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}
