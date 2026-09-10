package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	gm "github.com/palchrb/garmin-messenger-relay/internal/hermes"
)

// fakeSender records emails and can fail the first n sends.
type fakeSender struct {
	mu       sync.Mutex
	sent     []Message
	failNext int
}

func (f *fakeSender) Send(m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return errDial
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeSender) SendContext(_ context.Context, m Message) error { return f.Send(m) }

func (f *fakeSender) messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.sent...)
}

var errDial = &testErr{"dial tcp: connection refused"}

type testErr struct{ s string }

func (e *testErr) Error() string { return e.s }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Garmin.Phone = "+15559876543"
	cfg.Garmin.SessionDir = t.TempDir()
	cfg.SMTP.Host = "smtp.example.com"
	cfg.SMTP.From = "relay@example.com"
	cfg.IMAP.Host = "imap.example.com"
	cfg.IMAP.Username = "u"
	cfg.IMAP.Password = "p"
	cfg.Forwarding.DefaultRecipients = []string{"a@example.com", "b@example.com"}
	return &cfg
}

// newTestRelay builds a Relay whose Hermes API talks to handler.
func newTestRelay(t *testing.T, cfg *Config, handler http.Handler) (*Relay, *fakeSender) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	auth := gm.NewHermesAuth(gm.WithHermesBase(srv.URL), gm.WithSessionDir(cfg.Garmin.SessionDir))
	auth.AccessToken = "token"
	auth.ExpiresAt = float64(time.Now().Add(time.Hour).Unix())

	store, err := NewMsgStore(cfg.Garmin.SessionDir, cfg.IMAP.ReplyWindowDays, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(cfg.Garmin.SessionDir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	r := &Relay{
		cfg:              cfg,
		auth:             auth,
		api:              gm.NewHermesAPI(auth),
		sr:               gm.NewHermesSignalR(auth),
		mailer:           sender,
		store:            store,
		state:            state,
		alerter:          NewAlerter(cfg.Alerts, sender, testLogger()),
		log:              testLogger(),
		inbox:            make(chan gm.MessageModel, inboxSize),
		catchupCh:        make(chan struct{}, 1),
		noticeSent:       make(map[string]time.Time),
		deliveryFailures: make(map[uuid.UUID]int),
		workerDone:       make(chan struct{}),
	}
	return r, sender
}

func init() {
	// Keep retries fast in tests.
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
}

func strPtr(s string) *string { return &s }

func TestResolveRecipients(t *testing.T) {
	cfg := testConfig(t)
	cfg.Forwarding.CaptionRouting = true
	cfg.Forwarding.CaptionRoutingAllowlist = []string{"@example.org"}
	r, _ := newTestRelay(t, cfg, http.NotFoundHandler())

	got := r.resolveRecipients("hi c@example.org and evil@spam.com and a@example.com")
	want := []string{"a@example.com", "b@example.com", "c@example.org"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}

	cfg.Forwarding.CaptionRoutingReplacesDefault = true
	if got := r.resolveRecipients("only c@example.org"); len(got) != 1 || got[0] != "c@example.org" {
		t.Fatalf("replace mode got %v", got)
	}
	if got := r.resolveRecipients("only evil@spam.com"); len(got) != 2 {
		t.Fatalf("disallowed caption address must fall back to defaults, got %v", got)
	}

	cfg.Forwarding.CaptionRouting = false
	if got := r.resolveRecipients("c@example.org"); len(got) != 2 {
		t.Fatalf("caption routing off got %v", got)
	}
}

func TestShouldSkip(t *testing.T) {
	cfg := testConfig(t)
	cfg.Forwarding.AllowedSenders = []string{"+15551234567"}
	r, _ := newTestRelay(t, cfg, http.NotFoundHandler())

	reaction := gm.HermesMessageTypeReaction
	cases := []struct {
		name string
		msg  gm.MessageModel
		skip bool
	}{
		{"allowed sender", gm.MessageModel{From: strPtr("+15551234567")}, false},
		{"own phone", gm.MessageModel{From: strPtr("+15559876543")}, true},
		{"unknown sender", gm.MessageModel{From: strPtr("+15550000000")}, true},
		{"reaction", gm.MessageModel{From: strPtr("+15551234567"), MessageType: &reaction}, true},
	}
	for _, c := range cases {
		if skip, _ := r.shouldSkip(c.msg); skip != c.skip {
			t.Errorf("%s: skip=%v want %v", c.name, skip, c.skip)
		}
	}
}

func TestBuildReplyCaption(t *testing.T) {
	long := strings.Repeat("ø", 2000)
	got := buildReplyCaption(InboundReply{From: "alice@example.com", FromName: "Alice", TextBody: long})
	if !strings.HasPrefix(got, "Alice: ") {
		t.Fatalf("missing sender prefix: %q", got[:20])
	}
	if n := len([]rune(got)); n != maxReplyChars {
		t.Fatalf("caption has %d runes, want %d", n, maxReplyChars)
	}
	if buildReplyCaption(InboundReply{From: "a@b", TextBody: "  "}) != "" {
		t.Fatal("blank body should give empty caption")
	}
	if got := buildReplyCaption(InboundReply{From: "a@b", TextBody: "hi"}); got != "a@b: hi" {
		t.Fatalf("fallback to address: %q", got)
	}
}

// hermesHandler serves the subset of the Hermes API the relay uses.
func hermesHandler(t *testing.T, sends *[]gm.SendMessageRequest, delivered *[]string) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case req.Method == "GET" && req.URL.Path == "/Conversation/Updated":
			_, _ = w.Write(fixture(t, "get_conversations.json"))
		case req.Method == "GET" && strings.HasPrefix(req.URL.Path, "/Conversation/Details/"):
			_, _ = w.Write(fixture(t, "conversation_detail.json"))
		case req.Method == "PUT" && strings.HasPrefix(req.URL.Path, "/Status/Delivered/"):
			if delivered != nil {
				*delivered = append(*delivered, req.URL.Path)
			}
			_, _ = w.Write([]byte(`{}`))
		case req.Method == "POST" && req.URL.Path == "/Message/Send":
			var body gm.SendMessageRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Errorf("decode send: %v", err)
			}
			if sends != nil {
				*sends = append(*sends, body)
			}
			_, _ = w.Write(fixture(t, "send_message_response.json"))
		default:
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(404)
		}
	})
}

func TestProcessMessageOneEmailPerRecipient(t *testing.T) {
	cfg := testConfig(t)
	cfg.Forwarding.Contacts = map[string]string{"+15551234567": "Ola"}
	var delivered []string
	r, sender := newTestRelay(t, cfg, hermesHandler(t, nil, &delivered))

	msgID := uuid.New()
	sent := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	msg := gm.MessageModel{
		MessageID:      msgID,
		ConversationID: uuid.New(),
		From:           strPtr("+15551234567"),
		MessageBody:    strPtr("Hei fra fjellet"),
		SentAt:         &sent,
	}
	r.processMessage(context.Background(), msg)

	emails := sender.messages()
	if len(emails) != 2 {
		t.Fatalf("expected one email per recipient, got %d", len(emails))
	}
	if emails[0].MessageID == emails[1].MessageID {
		t.Fatal("each recipient must get a distinct Message-ID")
	}
	for _, e := range emails {
		if len(e.To) != 1 {
			t.Errorf("email should have exactly one recipient, got %v", e.To)
		}
		if !strings.Contains(e.Subject, "Ola (+15551234567)") {
			t.Errorf("subject should use contact name: %q", e.Subject)
		}
		if !strings.Contains(e.Body, "Caption: Hei fra fjellet") {
			t.Errorf("body missing caption: %q", e.Body)
		}
		if _, ok := r.store.Lookup(e.MessageID); !ok {
			t.Errorf("sent message %s not recorded for reply routing", e.MessageID)
		}
	}
	if !r.state.IsProcessed(msgID) {
		t.Fatal("message should be marked processed")
	}
	if len(delivered) != 1 {
		t.Fatalf("expected one MarkAsDelivered call, got %v", delivered)
	}

	// Re-processing (e.g. from catch-up) must be a no-op.
	r.processMessage(context.Background(), msg)
	if len(sender.messages()) != 2 {
		t.Fatal("duplicate delivery on reprocess")
	}
}

func TestProcessMessageRetriesThenLeavesUnprocessed(t *testing.T) {
	cfg := testConfig(t)
	cfg.Forwarding.DefaultRecipients = []string{"a@example.com"}
	cfg.Alerts.Email = "admin@example.com"
	r, sender := newTestRelay(t, cfg, hermesHandler(t, nil, nil))
	r.alerter = NewAlerter(cfg.Alerts, sender, testLogger())

	msg := gm.MessageModel{MessageID: uuid.New(), ConversationID: uuid.New(), From: strPtr("+15551234567"), MessageBody: strPtr("x")}

	// Fail all three attempts, then the alert email goes through.
	sender.failNext = len(retryDelays) + 1
	r.processMessage(context.Background(), msg)
	if r.state.IsProcessed(msg.MessageID) {
		t.Fatal("message must stay unprocessed after total delivery failure so catch-up retries it")
	}
	got := sender.messages()
	if len(got) != 1 || !strings.Contains(got[0].Subject, "Email delivery failed") {
		t.Fatalf("expected an operator alert, got %+v", got)
	}

	// Transient failure on first attempt, success on retry.
	sender.failNext = 1
	r.processMessage(context.Background(), msg)
	if !r.state.IsProcessed(msg.MessageID) {
		t.Fatal("message should be processed after a successful retry")
	}
	got = sender.messages()
	if len(got) != 2 || got[1].To[0] != "a@example.com" {
		t.Fatalf("expected the forwarded email, got %+v", got)
	}
}

func TestRunCatchupEnqueuesUnprocessedForeignMessages(t *testing.T) {
	cfg := testConfig(t)
	r, _ := newTestRelay(t, cfg, hermesHandler(t, nil, nil))
	// The fixture messages are from 2025; move the anchor before them.
	r.state.data.LastCatchupOK = time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC)

	if err := r.runCatchup(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []gm.MessageModel
	for len(r.inbox) > 0 {
		got = append(got, <-r.inbox)
	}
	// The fixture has one message from the other party and one from our own
	// phone (+15559876543), which must be skipped.
	if len(got) != 1 || derefStr(got[0].From) != "+15551234567" {
		t.Fatalf("expected exactly the foreign message, got %+v", got)
	}
	if got[0].ConversationID.String() != "a1b2c3d4-e5f6-7890-abcd-ef1234567890" {
		t.Fatalf("conversation id not propagated: %s", got[0].ConversationID)
	}

	// A second pass after processing must not enqueue it again, and with
	// nothing pending the anchor advances to now.
	r.state.MarkProcessed(got[0].MessageID)
	if err := r.runCatchup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.inbox) != 0 {
		t.Fatal("processed message re-enqueued")
	}
	if time.Since(r.state.LastCatchupOK()) > time.Minute {
		t.Fatal("catch-up with nothing pending should advance the anchor")
	}
}

func TestHandleReplyRoutesTextToGarmin(t *testing.T) {
	cfg := testConfig(t)
	var sends []gm.SendMessageRequest
	r, sender := newTestRelay(t, cfg, hermesHandler(t, &sends, nil))

	conv := uuid.New()
	if err := r.store.Add(SentMessage{MessageID: "<gmr-1@x>", ConversationID: conv, SenderPhone: "+15551234567", RecipientEmail: "alice@example.com", SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	r.handleReply(context.Background(), InboundReply{InReplyTo: "<gmr-1@x>", From: "Alice@Example.com", FromName: "Alice", TextBody: "Vi kommer kl 12"})
	if len(sends) != 1 {
		t.Fatalf("expected one Garmin send, got %d", len(sends))
	}
	if sends[0].MessageBody != "Alice: Vi kommer kl 12" || len(sends[0].To) != 1 || sends[0].To[0] != "+15551234567" {
		t.Fatalf("unexpected send %+v", sends[0])
	}
	if len(sender.messages()) != 0 {
		t.Fatal("no failure notice expected on success")
	}

	// Wrong sender: silently ignored, no notice (address may be forged).
	r.handleReply(context.Background(), InboundReply{InReplyTo: "<gmr-1@x>", From: "mallory@example.com", TextBody: "hi"})
	if len(sends) != 1 || len(sender.messages()) != 0 {
		t.Fatal("mismatched sender must be ignored silently")
	}

	// Unknown Message-ID from a stranger: no notice (mail reflector guard).
	r.handleReply(context.Background(), InboundReply{InReplyTo: "<gmr-unknown@x>", From: "stranger@example.net", MessageID: "<s1@example.net>", Subject: "spam", TextBody: "late"})
	if len(sender.messages()) != 0 {
		t.Fatal("unknown senders must not receive notices")
	}

	// Unknown Message-ID from a known recipient: notice threaded to their
	// email, with a fixed subject and no relay marker.
	r.handleReply(context.Background(), InboundReply{InReplyTo: "<gmr-unknown@x>", From: "alice@example.com", MessageID: "<r1@example.com>", Subject: "Re: hi", TextBody: "late"})
	notices := sender.messages()
	if len(notices) != 1 || notices[0].InReplyTo != "<r1@example.com>" || strings.Contains(notices[0].MessageID, "gmr-") || strings.Contains(notices[0].Subject, "hi") {
		t.Fatalf("expected a failure notice without the relay marker or echoed subject, got %+v", notices)
	}

	// Second notice to the same address within the cooldown is suppressed.
	r.handleReply(context.Background(), InboundReply{InReplyTo: "<gmr-unknown@x>", From: "alice@example.com", MessageID: "<r1b@example.com>", TextBody: "late again"})
	if len(sender.messages()) != 1 {
		t.Fatal("notice cooldown not applied")
	}
	r.noticeSent = map[string]time.Time{}

	// Reply with only an unsupported attachment: notice lists it.
	r.handleReply(context.Background(), InboundReply{InReplyTo: "<gmr-1@x>", From: "alice@example.com", MessageID: "<r2@example.com>", SkippedAttachments: []string{"doc.pdf (unsupported type application/pdf)"}})
	notices = sender.messages()
	if len(notices) != 2 || !strings.Contains(notices[1].Body, "doc.pdf") {
		t.Fatalf("expected notice about skipped attachment, got %+v", notices)
	}
	if len(sends) != 1 {
		t.Fatal("nothing should be sent to Garmin for an empty reply")
	}
}

func TestEnqueueDropsWhenFull(t *testing.T) {
	r, _ := newTestRelay(t, testConfig(t), http.NotFoundHandler())
	for i := 0; i < inboxSize+5; i++ {
		r.enqueue(gm.MessageModel{MessageID: uuid.New()})
	}
	if len(r.inbox) != inboxSize {
		t.Fatalf("inbox len %d", len(r.inbox))
	}
	r.triggerCatchup()
	r.triggerCatchup() // must not block
}

func TestHandleReplyRetriesWithSameUUID(t *testing.T) {
	cfg := testConfig(t)
	var sends []gm.SendMessageRequest
	var mu sync.Mutex
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.Method == "POST" && req.URL.Path == "/Message/Send" {
			var body gm.SendMessageRequest
			_ = json.NewDecoder(req.Body).Decode(&body)
			sends = append(sends, body)
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write(fixture(t, "send_message_response.json"))
			return
		}
		w.WriteHeader(404)
	})
	r, _ := newTestRelay(t, cfg, handler)
	if err := r.store.Add(SentMessage{MessageID: "<gmr-1@x>", SenderPhone: "+15551234567", RecipientEmail: "alice@example.com", SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r.handleReply(context.Background(), InboundReply{InReplyTo: "<gmr-1@x>", From: "alice@example.com", TextBody: "hi"})
	mu.Lock()
	defer mu.Unlock()
	if len(sends) != 2 {
		t.Fatalf("expected a retry, got %d sends", len(sends))
	}
	if sends[0].UUID == nil || sends[1].UUID == nil || *sends[0].UUID != *sends[1].UUID || *sends[0].OtaUuid != *sends[1].OtaUuid {
		t.Fatal("retry must reuse the message UUIDs so Garmin can deduplicate")
	}
}

func TestRunCatchupAnchorStaysBehindUndelivered(t *testing.T) {
	cfg := testConfig(t)
	r, _ := newTestRelay(t, cfg, hermesHandler(t, nil, nil))
	// Pretend a pass completed earlier so the margin applies.
	r.state.SetLastCatchupOK(time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC))

	if err := r.runCatchup(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The fixture message (2025-01-15T10:30Z) is enqueued but not yet
	// delivered, so the anchor must not move past it. The 24h lookback cap
	// applies since the fixture is old.
	anchor := r.state.LastCatchupOK()
	if anchor.After(time.Now().Add(-maxCatchupLookback + time.Minute)) {
		t.Fatalf("anchor advanced past undelivered message: %v", anchor)
	}
	if r.state.FirstRun() {
		t.Fatal("state should no longer be first-run")
	}
}

func TestRunCatchupFirstRunUsesNoMargin(t *testing.T) {
	cfg := testConfig(t)
	r, _ := newTestRelay(t, cfg, hermesHandler(t, nil, nil))
	if !r.state.FirstRun() {
		t.Fatal("fresh state should be first-run")
	}
	if err := r.runCatchup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.inbox) != 0 {
		t.Fatal("first run must not backfill history")
	}
}

func TestProcessMessageGivesUpAfterMaxPasses(t *testing.T) {
	cfg := testConfig(t)
	cfg.Forwarding.DefaultRecipients = []string{"a@example.com"}
	r, sender := newTestRelay(t, cfg, hermesHandler(t, nil, nil))
	msg := gm.MessageModel{MessageID: uuid.New(), ConversationID: uuid.New(), From: strPtr("+15551234567"), MessageBody: strPtr("x")}
	for i := 0; i < maxDeliveryPasses; i++ {
		sender.failNext = len(retryDelays) + 1
		r.processMessage(context.Background(), msg)
		if i < maxDeliveryPasses-1 && r.state.IsProcessed(msg.MessageID) {
			t.Fatalf("gave up too early on pass %d", i+1)
		}
	}
	if !r.state.IsProcessed(msg.MessageID) {
		t.Fatal("message should be marked processed after giving up")
	}
}
