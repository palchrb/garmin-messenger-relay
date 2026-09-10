package garminmessenger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/philippseith/signalr"
)

// HermesSignalROption configures HermesSignalR.
type HermesSignalROption func(*HermesSignalR)

// WithSignalRLogger sets a custom logger.
func WithSignalRLogger(logger *slog.Logger) HermesSignalROption {
	return func(sr *HermesSignalR) {
		sr.logger = logger
	}
}

// HermesSignalR is the real-time WebSocket client for Hermes events.
type HermesSignalR struct {
	auth   *HermesAuth
	client signalr.Client
	logger *slog.Logger

	onMessage                  func(MessageModel)
	onStatusUpdate             func(MessageStatusUpdate)
	onMuteUpdate               func(ConversationMuteStatusUpdate)
	onBlockUpdate              func(UserBlockStatusUpdate)
	onNotification             func(ServerNotification)
	onNonconversationalMessage func(string)
	// onReaction is called for dedicated reaction hub method invocations
	// (ReceiveReaction), if Garmin uses a separate hub method for reactions.
	onReaction func(MessageModel)
	onOpen     func()
	onClose    func()
	onError    func(error)

	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
}

// NewHermesSignalR creates a new SignalR client.
func NewHermesSignalR(auth *HermesAuth, opts ...HermesSignalROption) *HermesSignalR {
	sr := &HermesSignalR{
		auth:   auth,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(sr)
	}
	return sr
}

// Handler registration

func (sr *HermesSignalR) OnMessage(handler func(MessageModel)) { sr.onMessage = handler }
func (sr *HermesSignalR) OnStatusUpdate(handler func(MessageStatusUpdate)) {
	sr.onStatusUpdate = handler
}
func (sr *HermesSignalR) OnMuteUpdate(handler func(ConversationMuteStatusUpdate)) {
	sr.onMuteUpdate = handler
}
func (sr *HermesSignalR) OnBlockUpdate(handler func(UserBlockStatusUpdate)) {
	sr.onBlockUpdate = handler
}
func (sr *HermesSignalR) OnNotification(handler func(ServerNotification)) {
	sr.onNotification = handler
}
func (sr *HermesSignalR) OnNonconversationalMessage(handler func(string)) {
	sr.onNonconversationalMessage = handler
}
func (sr *HermesSignalR) OnReaction(handler func(MessageModel)) { sr.onReaction = handler }
func (sr *HermesSignalR) OnOpen(handler func())                 { sr.onOpen = handler }
func (sr *HermesSignalR) OnClose(handler func())                { sr.onClose = handler }
func (sr *HermesSignalR) OnError(handler func(error))           { sr.onError = handler }

// initialConnectTimeout bounds how long Start waits for the first
// successful connection before giving up so that misconfiguration fails fast.
const initialConnectTimeout = 30 * time.Second

// newReconnectBackoff returns the backoff policy for reconnects. The
// library's default gives up after 15 minutes, which would leave the relay
// alive but permanently disconnected; we never give up and let the
// supervisor decide when to restart the process.
func newReconnectBackoff() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = 1 * time.Second
	b.MaxInterval = 60 * time.Second
	b.MaxElapsedTime = 0
	return b
}

// stateEvent describes the callback to run for a client state transition.
type stateEvent int

const (
	eventNone stateEvent = iota
	eventOpen
	eventClose
	eventClosed
)

// classifyTransition maps a state transition to the relay-level event that
// should be raised. It is pure so it can be unit-tested.
func classifyTransition(prev, cur signalr.ClientState) stateEvent {
	if prev == cur {
		return eventNone
	}
	switch cur {
	case signalr.ClientConnected:
		return eventOpen
	case signalr.ClientClosed:
		return eventClosed
	case signalr.ClientConnecting:
		if prev == signalr.ClientConnected {
			return eventClose
		}
	}
	return eventNone
}

// Start builds and starts the SignalR connection and blocks until ctx is
// cancelled. Reconnects are handled by the library with an unbounded backoff;
// OnOpen fires on every (re)connect and OnClose whenever an established
// connection is lost. If the client closes permanently while ctx is still
// live (server refused reconnection), Start returns an error so the process
// can exit and be restarted by its supervisor.
func (sr *HermesSignalR) Start(ctx context.Context) error {
	sr.mu.Lock()
	sr.stopped = false
	sr.mu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	sr.cancel = cancel

	receiver := &hermesReceiver{sr: sr}

	hubURL := sr.auth.HermesBase + "/messaging"
	sr.logger.Debug("Building SignalR hub", "url", hubURL)

	client, err := signalr.NewClient(ctx,
		signalr.WithConnector(func() (signalr.Connection, error) {
			return sr.connect(ctx, hubURL)
		}),
		signalr.WithReceiver(receiver),
		signalr.WithBackoff(newReconnectBackoff),
		signalr.Logger(&slogAdapter{logger: sr.logger}, false),
		signalr.KeepAliveInterval(15*time.Second),
		signalr.TimeoutInterval(30*time.Second),
	)
	if err != nil {
		cancel()
		return fmt.Errorf("creating SignalR client: %w", err)
	}

	sr.mu.Lock()
	sr.client = client
	sr.mu.Unlock()

	// The library notifies observers from goroutines that hold the client
	// mutex while sending, so this channel must be drained promptly for the
	// whole client lifetime and the callbacks below must never block.
	stateCh := make(chan signalr.ClientState, 16)
	cancelObserve := client.ObserveStateChanged(stateCh)
	// ctx must be cancelled before the observation is removed, otherwise a
	// notifier blocked on the channel would deadlock with cancelObserve.
	defer func() {
		cancel()
		cancelObserve()
	}()

	client.Start()

	initial := time.NewTimer(initialConnectTimeout)
	defer initial.Stop()
	connectedOnce := false
	prev := signalr.ClientCreated

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-initial.C:
			if !connectedOnce {
				return fmt.Errorf("waiting for SignalR connection: %w", errOrTimeout(client.Err()))
			}

		case st := <-stateCh:
			// Handle the delivered state first so a quick
			// Connected->Connecting->Connected cycle is not coalesced away
			// (which would skip the reconnect catch-up), then reconcile
			// with the live state in case notifications arrived out of
			// order.
			for _, cur := range []signalr.ClientState{st, client.State()} {
				ev := classifyTransition(prev, cur)
				prev = cur
				switch ev {
				case eventOpen:
					connectedOnce = true
					sr.logger.Debug("SignalR connected")
					if sr.onOpen != nil {
						sr.onOpen()
					}
				case eventClose:
					sr.logger.Warn("SignalR connection lost, reconnecting", "error", client.Err())
					if sr.onClose != nil {
						sr.onClose()
					}
				case eventClosed:
					if ctx.Err() != nil {
						return nil
					}
					return fmt.Errorf("SignalR client closed permanently: %w", errOrTimeout(client.Err()))
				}
			}
		}
	}
}

func errOrTimeout(err error) error {
	if err == nil {
		return fmt.Errorf("no connection within %s", initialConnectTimeout)
	}
	return err
}

// connect performs SignalR negotiate and creates a WebSocket connection.
// This mirrors the Python signalrcore approach: negotiate to get connectionId,
// then open a WebSocket directly (ignoring the server's availableTransports list).
func (sr *HermesSignalR) connect(ctx context.Context, hubURL string) (signalr.Connection, error) {
	sr.logger.Log(context.TODO(), LevelTrace, "SignalR connector: requesting access token")
	token, err := sr.auth.AccessTokenFactory(ctx)
	if err != nil {
		if sr.onError != nil {
			sr.onError(fmt.Errorf("getting access token: %w", err))
		}
		return nil, fmt.Errorf("getting access token: %w", err)
	}
	sr.logger.Log(context.TODO(), LevelTrace, "SignalR connector: token obtained", "length", len(token))

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)

	// 1. Negotiate
	negotiateURL := hubURL + "/negotiate"
	sr.logger.Log(context.TODO(), LevelTrace, "SignalR connector: negotiate", "url", negotiateURL)

	negCtx, negCancel := context.WithTimeout(ctx, 30*time.Second)
	defer negCancel()
	negReq, err := http.NewRequestWithContext(negCtx, "POST", negotiateURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating negotiate request: %w", err)
	}
	negReq.Header = headers.Clone()

	resp, err := sr.httpClient().Do(negReq)
	if err != nil {
		return nil, fmt.Errorf("negotiate request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	sr.logger.Log(context.TODO(), LevelTrace, "SignalR connector: negotiate response",
		"status", resp.StatusCode, "body", truncate(maskSecrets(string(body)), 2000))

	if resp.StatusCode != 200 {
		err := &APIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: truncate(string(body), 500), URL: negotiateURL, Method: "POST"}
		if sr.onError != nil {
			sr.onError(fmt.Errorf("negotiate failed: %w", err))
		}
		return nil, fmt.Errorf("negotiate failed: %w", err)
	}

	var negResp struct {
		ConnectionID string `json:"connectionId"`
		URL          string `json:"url"`
		AccessToken  string `json:"accessToken"`
	}
	if err := json.Unmarshal(body, &negResp); err != nil {
		return nil, fmt.Errorf("parsing negotiate response: %w", err)
	}

	// 2. Build WebSocket URL.
	// Azure SignalR Service: negotiate returns a redirect URL + new access token.
	// Standard SignalR: negotiate returns connectionId, connect back to same host.
	var wsURL *url.URL
	if negResp.URL != "" && negResp.AccessToken != "" {
		sr.logger.Log(context.TODO(), LevelTrace, "SignalR connector: Azure redirect",
			"url", negResp.URL, "tokenLength", len(negResp.AccessToken))
		wsURL, err = url.Parse(negResp.URL)
		if err != nil {
			return nil, fmt.Errorf("parsing Azure redirect URL: %w", err)
		}
		// Use the Azure-issued token instead of our Hermes token
		headers.Set("Authorization", "Bearer "+negResp.AccessToken)
	} else {
		sr.logger.Log(context.TODO(), LevelTrace, "SignalR connector: standard negotiate", "connectionId", negResp.ConnectionID)
		wsURL, _ = url.Parse(hubURL)
		q := wsURL.Query()
		q.Set("id", negResp.ConnectionID)
		wsURL.RawQuery = q.Encode()
	}

	switch wsURL.Scheme {
	case "https":
		wsURL.Scheme = "wss"
	case "http":
		wsURL.Scheme = "ws"
	}

	connID := negResp.ConnectionID
	if connID == "" {
		connID = "azure"
	}

	sr.logger.Log(context.TODO(), LevelTrace, "SignalR connector: opening WebSocket", "url", wsURL.String())
	conn, err := signalr.NewWebSocketConnection(ctx, wsURL, connID, headers)
	if err != nil {
		return nil, fmt.Errorf("WebSocket dial: %w", err)
	}
	sr.logger.Log(context.TODO(), LevelTrace, "SignalR connector: connected", "connectionId", conn.ConnectionID())
	return conn, nil
}

// Stop disconnects the SignalR client.
func (sr *HermesSignalR) Stop() {
	sr.mu.Lock()
	sr.stopped = true
	sr.mu.Unlock()

	if sr.cancel != nil {
		sr.cancel()
	}
	sr.logger.Debug("SignalR stopped")
}

// httpClient returns the HTTP client used for negotiate requests.
func (sr *HermesSignalR) httpClient() *http.Client {
	if sr.auth != nil && sr.auth.httpClient != nil {
		return sr.auth.httpClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// currentClient returns the underlying client under lock.
func (sr *HermesSignalR) currentClient() signalr.Client {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.client
}

// Client-to-server invocations

// MarkAsDelivered invokes MarkAsDelivered on the server.
func (sr *HermesSignalR) MarkAsDelivered(conversationID, messageID uuid.UUID) {
	if client := sr.currentClient(); client != nil {
		sr.logger.Debug("SignalR Send", "method", "MarkAsDelivered", "conversationId", conversationID, "messageId", messageID)
		sr.logSendResult("MarkAsDelivered", client.Send("MarkAsDelivered", conversationID.String(), messageID.String()))
	}
}

// logSendResult reports a failed fire-and-forget Send without blocking the
// caller.
func (sr *HermesSignalR) logSendResult(method string, errCh <-chan error) {
	go func() {
		if err := <-errCh; err != nil {
			sr.logger.Warn("SignalR Send failed", "method", method, "error", err)
		}
	}()
}

// MarkAsRead invokes MarkAsRead on the server.
func (sr *HermesSignalR) MarkAsRead(conversationID, messageID uuid.UUID) {
	if client := sr.currentClient(); client != nil {
		sr.logger.Debug("SignalR Send", "method", "MarkAsRead", "conversationId", conversationID, "messageId", messageID)
		sr.logSendResult("MarkAsRead", client.Send("MarkAsRead", conversationID.String(), messageID.String()))
	}
}

// QueryNetworkProperties invokes NetworkProperties on the server
// and returns a channel that will receive the result.
func (sr *HermesSignalR) QueryNetworkProperties() <-chan *NetworkPropertiesResponse {
	ch := make(chan *NetworkPropertiesResponse, 1)
	if client := sr.currentClient(); client != nil {
		sr.logger.Debug("SignalR Invoke", "method", "NetworkProperties")
		result := <-client.Invoke("NetworkProperties")
		if result.Error != nil {
			sr.logger.Error("NetworkProperties invocation error", "error", result.Error)
			close(ch)
		} else {
			data, err := json.Marshal(result.Value)
			if err != nil {
				sr.logger.Error("Error marshaling NetworkProperties result", "error", err)
				close(ch)
			} else {
				var resp NetworkPropertiesResponse
				if err := json.Unmarshal(data, &resp); err != nil {
					sr.logger.Error("Error parsing NetworkProperties response", "error", err)
					close(ch)
				} else {
					ch <- &resp
				}
			}
		}
	} else {
		close(ch)
	}
	return ch
}

// hermesReceiver implements the receiver interface for the SignalR library.
// Method names match the SignalR hub method names exactly.
type hermesReceiver struct {
	sr *HermesSignalR
}

func (r *hermesReceiver) ReceiveMessage(raw json.RawMessage) {
	r.sr.logger.Log(context.TODO(), LevelTrace, "ReceiveMessage raw", "json", truncate(string(raw), 10000))
	var msg MessageModel
	if err := json.Unmarshal(raw, &msg); err != nil {
		r.sr.logger.Error("Error parsing ReceiveMessage", "error", err)
		return
	}
	r.sr.logger.Debug("ReceiveMessage", "messageId", msg.MessageID)
	if r.sr.onMessage != nil {
		r.sr.onMessage(msg)
	}
}

func (r *hermesReceiver) ReceiveMessageUpdate(raw json.RawMessage) {
	r.sr.logger.Log(context.TODO(), LevelTrace, "ReceiveMessageUpdate raw", "json", truncate(string(raw), 10000))
	var update MessageStatusUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		r.sr.logger.Error("Error parsing ReceiveMessageUpdate", "error", err)
		return
	}
	r.sr.logger.Debug("ReceiveMessageUpdate", "messageId", update.MessageID)
	if r.sr.onStatusUpdate != nil {
		r.sr.onStatusUpdate(update)
	}
}

func (r *hermesReceiver) ReceiveConversationMuteStatusUpdate(raw json.RawMessage) {
	r.sr.logger.Log(context.TODO(), LevelTrace, "ReceiveConversationMuteStatusUpdate raw", "json", truncate(string(raw), 10000))
	var update ConversationMuteStatusUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		r.sr.logger.Error("Error parsing ReceiveConversationMuteStatusUpdate", "error", err)
		return
	}
	if r.sr.onMuteUpdate != nil {
		r.sr.onMuteUpdate(update)
	}
}

func (r *hermesReceiver) ReceiveUserBlockStatusUpdate(raw json.RawMessage) {
	r.sr.logger.Log(context.TODO(), LevelTrace, "ReceiveUserBlockStatusUpdate raw", "json", truncate(string(raw), 10000))
	var update UserBlockStatusUpdate
	if err := json.Unmarshal(raw, &update); err != nil {
		r.sr.logger.Error("Error parsing ReceiveUserBlockStatusUpdate", "error", err)
		return
	}
	if r.sr.onBlockUpdate != nil {
		r.sr.onBlockUpdate(update)
	}
}

func (r *hermesReceiver) ReceiveServerNotification(raw json.RawMessage) {
	r.sr.logger.Log(context.TODO(), LevelTrace, "ReceiveServerNotification raw", "json", truncate(string(raw), 10000))
	var notif ServerNotification
	if err := json.Unmarshal(raw, &notif); err != nil {
		r.sr.logger.Error("Error parsing ReceiveServerNotification", "error", err)
		return
	}
	r.sr.logger.Debug("ServerNotification", "notification", notif)
	if r.sr.onNotification != nil {
		r.sr.onNotification(notif)
	}
}

// ReceiveReaction handles a dedicated reaction push from the Garmin SignalR hub.
// Garmin may send reactions via this separate hub method rather than (or in addition
// to) the regular ReceiveMessage handler. The payload uses the same MessageModel
// shape but with messageType="Reaction" and parentMessageId set.
func (r *hermesReceiver) ReceiveReaction(raw json.RawMessage) {
	r.sr.logger.Log(context.TODO(), LevelTrace, "ReceiveReaction raw", "json", truncate(string(raw), 10000))
	var msg MessageModel
	if err := json.Unmarshal(raw, &msg); err != nil {
		r.sr.logger.Error("Error parsing ReceiveReaction", "error", err)
		return
	}
	reactionType := HermesMessageTypeReaction
	msg.MessageType = &reactionType
	r.sr.logger.Debug("ReceiveReaction", "messageId", msg.MessageID, "parentMessageId", msg.ParentMessageID)
	if r.sr.onReaction != nil {
		r.sr.onReaction(msg)
	} else if r.sr.onMessage != nil {
		// Fall back to the regular message handler if no dedicated reaction handler
		// is registered, so reactions aren't silently lost.
		r.sr.onMessage(msg)
	}
}

func (r *hermesReceiver) ReceiveNonconversationalMessage(raw json.RawMessage) {
	var imei string
	// The raw value might be a string or a number (IMEI)
	if err := json.Unmarshal(raw, &imei); err != nil {
		// Try as a number
		var numIMEI int64
		if err2 := json.Unmarshal(raw, &numIMEI); err2 != nil {
			r.sr.logger.Error("Error parsing ReceiveNonconversationalMessage", "error", err, "raw", string(raw))
			return
		}
		imei = fmt.Sprintf("%d", numIMEI)
	}
	r.sr.logger.Debug("ReceiveNonconversationalMessage", "imei", imei)
	if r.sr.onNonconversationalMessage != nil {
		r.sr.onNonconversationalMessage(imei)
	}
}

// slogAdapter adapts slog.Logger to the SignalR library's go-kit/log interface.
// The library emits flat key-value pairs: "level", "debug", "ts", "...", "state", 1
type slogAdapter struct {
	logger *slog.Logger
}

func (a *slogAdapter) Log(keyVals ...interface{}) error {
	if len(keyVals) == 0 {
		return nil
	}
	// go-kit/log: all elements are key-value pairs.
	// Skip keys slog already manages (level, ts, caller).
	// Only the library's info logger is enabled (debug is off), so what
	// arrives here includes every reconnect failure and must be visible at
	// the default log level.
	var attrs []any
	hasError := false
	for i := 0; i+1 < len(keyVals); i += 2 {
		key := fmt.Sprint(keyVals[i])
		if key == "level" || key == "ts" || key == "caller" {
			continue
		}
		if key == "error" || key == "connect" {
			hasError = true
		}
		attrs = append(attrs, key, keyVals[i+1])
	}
	if hasError {
		a.logger.Warn("signalr", attrs...)
	} else {
		a.logger.Info("signalr", attrs...)
	}
	return nil
}
