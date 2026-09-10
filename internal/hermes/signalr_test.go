package garminmessenger

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/philippseith/signalr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test that receiver methods parse fixture data correctly

func TestReceiver_ReceiveMessage(t *testing.T) {
	var received *MessageModel
	sr := NewHermesSignalR(NewHermesAuth())
	sr.OnMessage(func(msg MessageModel) {
		received = &msg
	})
	receiver := &hermesReceiver{sr: sr}

	data := loadFixture(t, "message_full.json")
	receiver.ReceiveMessage(json.RawMessage(data))

	require.NotNil(t, received)
	assert.Equal(t, uuid.MustParse(testMsgID), received.MessageID)
	assert.Equal(t, ptr("Full message with all fields"), received.MessageBody)
	assert.Equal(t, ptr(testUserID), received.From)
}

func TestReceiver_ReceiveMessageUpdate(t *testing.T) {
	var received *MessageStatusUpdate
	sr := NewHermesSignalR(NewHermesAuth())
	sr.OnStatusUpdate(func(update MessageStatusUpdate) {
		received = &update
	})
	receiver := &hermesReceiver{sr: sr}

	data := loadFixture(t, "signalr_status_update.json")
	receiver.ReceiveMessageUpdate(json.RawMessage(data))

	require.NotNil(t, received)
	assert.Equal(t, uuid.MustParse(testMsgID), received.MessageID.MessageID)
	assert.Equal(t, ptr(MessageStatusDelivered), received.MessageStatus)
}

func TestReceiver_ReceiveConversationMuteStatusUpdate(t *testing.T) {
	var received *ConversationMuteStatusUpdate
	sr := NewHermesSignalR(NewHermesAuth())
	sr.OnMuteUpdate(func(update ConversationMuteStatusUpdate) {
		received = &update
	})
	receiver := &hermesReceiver{sr: sr}

	data := []byte(`{"conversationId":"` + testConvID + `","isMuted":true}`)
	receiver.ReceiveConversationMuteStatusUpdate(json.RawMessage(data))

	require.NotNil(t, received)
	assert.Equal(t, ptr(uuid.MustParse(testConvID)), received.ConversationID)
	assert.Equal(t, ptr(true), received.IsMuted)
}

func TestReceiver_ReceiveUserBlockStatusUpdate(t *testing.T) {
	var received *UserBlockStatusUpdate
	sr := NewHermesSignalR(NewHermesAuth())
	sr.OnBlockUpdate(func(update UserBlockStatusUpdate) {
		received = &update
	})
	receiver := &hermesReceiver{sr: sr}

	data := []byte(`{"userId":"user-123","isBlocked":true}`)
	receiver.ReceiveUserBlockStatusUpdate(json.RawMessage(data))

	require.NotNil(t, received)
	assert.Equal(t, ptr("user-123"), received.UserID)
	assert.Equal(t, ptr(true), received.IsBlocked)
}

func TestReceiver_ReceiveServerNotification(t *testing.T) {
	var received *ServerNotification
	sr := NewHermesSignalR(NewHermesAuth())
	sr.OnNotification(func(notif ServerNotification) {
		received = &notif
	})
	receiver := &hermesReceiver{sr: sr}

	data := []byte(`{"notificationType":"Maintenance","message":"Server restarting"}`)
	receiver.ReceiveServerNotification(json.RawMessage(data))

	require.NotNil(t, received)
	assert.Equal(t, ptr("Maintenance"), received.NotificationType)
	assert.Equal(t, ptr("Server restarting"), received.Message)
}

func TestReceiver_ReceiveNonconversationalMessage_String(t *testing.T) {
	var received string
	sr := NewHermesSignalR(NewHermesAuth())
	sr.OnNonconversationalMessage(func(imei string) {
		received = imei
	})
	receiver := &hermesReceiver{sr: sr}

	data := []byte(`"300234063904190"`)
	receiver.ReceiveNonconversationalMessage(json.RawMessage(data))

	assert.Equal(t, "300234063904190", received)
}

func TestReceiver_ReceiveNonconversationalMessage_Number(t *testing.T) {
	var received string
	sr := NewHermesSignalR(NewHermesAuth())
	sr.OnNonconversationalMessage(func(imei string) {
		received = imei
	})
	receiver := &hermesReceiver{sr: sr}

	data := []byte(`300234063904190`)
	receiver.ReceiveNonconversationalMessage(json.RawMessage(data))

	assert.Equal(t, "300234063904190", received)
}

func TestReceiver_NilHandlerNoPanic(t *testing.T) {
	sr := NewHermesSignalR(NewHermesAuth())
	receiver := &hermesReceiver{sr: sr}

	// All handlers are nil — should not panic
	assert.NotPanics(t, func() {
		receiver.ReceiveMessage(json.RawMessage(loadFixture(t, "message_simple.json")))
		receiver.ReceiveMessageUpdate(json.RawMessage(loadFixture(t, "signalr_status_update.json")))
		receiver.ReceiveConversationMuteStatusUpdate(json.RawMessage(`{}`))
		receiver.ReceiveUserBlockStatusUpdate(json.RawMessage(`{}`))
		receiver.ReceiveServerNotification(json.RawMessage(`{}`))
		receiver.ReceiveNonconversationalMessage(json.RawMessage(`"123"`))
	})
}

func TestReceiver_BadData(t *testing.T) {
	sr := NewHermesSignalR(NewHermesAuth())
	receiver := &hermesReceiver{sr: sr}

	// Invalid JSON — should log error, not panic
	assert.NotPanics(t, func() {
		receiver.ReceiveMessage(json.RawMessage(`{invalid`))
		receiver.ReceiveMessageUpdate(json.RawMessage(`{invalid`))
		receiver.ReceiveConversationMuteStatusUpdate(json.RawMessage(`{invalid`))
		receiver.ReceiveUserBlockStatusUpdate(json.RawMessage(`{invalid`))
		receiver.ReceiveServerNotification(json.RawMessage(`{invalid`))
		receiver.ReceiveNonconversationalMessage(json.RawMessage(`{invalid`))
	})
}

func TestSlogAdapter(t *testing.T) {
	adapter := &slogAdapter{logger: slog.Default()}
	assert.NotPanics(t, func() {
		_ = adapter.Log("test message", "key", "value")
		_ = adapter.Log()
	})
}

func TestClassifyTransition(t *testing.T) {
	cases := []struct {
		prev, cur signalr.ClientState
		want      stateEvent
	}{
		{signalr.ClientCreated, signalr.ClientConnecting, eventNone},
		{signalr.ClientConnecting, signalr.ClientConnected, eventOpen},
		{signalr.ClientConnected, signalr.ClientConnecting, eventClose},
		{signalr.ClientConnecting, signalr.ClientConnecting, eventNone},
		{signalr.ClientConnected, signalr.ClientConnected, eventNone},
		{signalr.ClientConnecting, signalr.ClientClosed, eventClosed},
		{signalr.ClientConnected, signalr.ClientClosed, eventClosed},
	}
	for _, c := range cases {
		if got := classifyTransition(c.prev, c.cur); got != c.want {
			t.Errorf("classifyTransition(%v, %v) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

func TestReconnectBackoffNeverStops(t *testing.T) {
	b := newReconnectBackoff()
	for i := 0; i < 50; i++ {
		if d := b.NextBackOff(); d == backoff.Stop {
			t.Fatalf("backoff gave up after %d attempts", i)
		} else if d > 90*time.Second { // MaxInterval plus randomisation
			t.Fatalf("backoff interval %v exceeds cap", d)
		}
	}
}
