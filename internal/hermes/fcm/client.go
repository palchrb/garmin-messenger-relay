package fcm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gm "github.com/palchrb/garmin-messenger-relay/internal/hermes"
	"github.com/palchrb/garmin-messenger-relay/internal/hermes/internal/mcspb"
)

// Event is a marker interface for FCM push notification events.
type Event interface {
	fcmEvent()
}

// NewMessage wraps a MessageModel received via FCM push notification.
type NewMessage struct {
	gm.MessageModel
}

func (NewMessage) fcmEvent() {}

// NonconversationalMessage represents a device event via FCM.
type NonconversationalMessage struct {
	IMEI string
}

func (NonconversationalMessage) fcmEvent() {}

// DeviceAccountUpdate represents a device account update via FCM.
type DeviceAccountUpdate struct {
	Data json.RawMessage
}

func (DeviceAccountUpdate) fcmEvent() {}

// Credentials holds Android-native FCM registration credentials.
// Note: Web-style credentials (with private_key and auth_secret) are no longer supported.
type Credentials struct {
	Raw           json.RawMessage `json:"raw"` // GCM credentials (androidId, securityToken)
	Token         string          `json:"token"`
	PersistentIDs []string        `json:"persistent_ids"`
}

// Option configures Client.
type Option func(*Client)

// WithLogger sets a custom logger for Client.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Client) {
		c.logger = logger
	}
}

// WithHTTPClient sets a custom HTTP client for FCM registration.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		c.httpClient = client
	}
}

// Client manages FCM registration and MCS push notification listening.
type Client struct {
	credentials *Credentials
	sessionDir  string
	logger      *slog.Logger
	httpClient  *http.Client
	mu          sync.Mutex

	// dialMCS is overridable for testing (returns a conn to MCS server).
	dialMCS func(ctx context.Context) (io.ReadWriteCloser, error)

	onMessage                  func(NewMessage)
	onNonconversationalMessage func(NonconversationalMessage)
	onDeviceAccountUpdate      func(DeviceAccountUpdate)
	onConnected                func()
	onDisconnected             func()
	onError                    func(error)
}

// NewClient creates a new Client.
func NewClient(sessionDir string, opts ...Option) *Client {
	c := &Client{
		sessionDir: sessionDir,
		logger:     slog.Default(),
		httpClient: http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Token returns the current FCM token (empty if not registered).
func (c *Client) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credentials == nil {
		return ""
	}
	return c.credentials.Token
}

// Credentials returns a copy of the current FCM credentials (nil if not registered).
// The returned value is safe to read without synchronization but must not be
// used to mutate internal state.
func (c *Client) Credentials() *Credentials {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credentials == nil {
		return nil
	}
	cpy := *c.credentials
	cpy.PersistentIDs = make([]string, len(c.credentials.PersistentIDs))
	copy(cpy.PersistentIDs, c.credentials.PersistentIDs)
	cpy.Raw = make(json.RawMessage, len(c.credentials.Raw))
	copy(cpy.Raw, c.credentials.Raw)
	return &cpy
}

// OnMessage registers a callback for new message events.
// Must be called before Listen().
func (c *Client) OnMessage(fn func(NewMessage)) { c.onMessage = fn }

// OnNonconversationalMessage registers a callback for non-conversational events.
// Must be called before Listen().
func (c *Client) OnNonconversationalMessage(fn func(NonconversationalMessage)) {
	c.onNonconversationalMessage = fn
}

// OnDeviceAccountUpdate registers a callback for device account update events.
// Must be called before Listen().
func (c *Client) OnDeviceAccountUpdate(fn func(DeviceAccountUpdate)) {
	c.onDeviceAccountUpdate = fn
}

// OnConnected registers a callback invoked when MCS connection is established.
// Must be called before Listen().
func (c *Client) OnConnected(fn func()) { c.onConnected = fn }

// OnDisconnected registers a callback invoked when MCS connection drops.
// Must be called before Listen().
func (c *Client) OnDisconnected(fn func()) { c.onDisconnected = fn }

// OnError registers a callback invoked for listener errors.
// Must be called before Listen().
func (c *Client) OnError(fn func(error)) { c.onError = fn }

// Register performs Android-native FCM registration and persists credentials.
// If credentials already exist on disk with a token, it returns that token.
func (c *Client) Register(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.credentials != nil && c.credentials.Token != "" {
		return c.credentials.Token, nil
	}

	if err := c.loadCredentials(); err == nil && c.credentials != nil && c.credentials.Token != "" {
		c.logger.Debug("FCM credentials already exist, reusing token")
		return c.credentials.Token, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		c.logger.Warn("failed to load persisted FCM credentials; attempting fresh registration", "error", err)
	}

	c.logger.Debug("Starting Android-native FCM registration", "sender_id", GarminSenderID)
	httpClient := c.loggingHTTPClient()

	// Step 1: GCM Checkin (Android device)
	device := DefaultAndroidDevice()
	androidID, securityToken, err := gcmCheckin(ctx, httpClient, 0, 0, device)
	if err != nil {
		return "", fmt.Errorf("FCM registration failed (checkin): %w", err)
	}
	c.logger.Debug("GCM checkin complete", "androidId", androidID)

	// Step 2: GCM Register (Android-native with APK cert)
	// For Android-native registration, the GCM token IS the FCM token
	fcmToken, err := gcmRegister(ctx, httpClient, androidID, securityToken, device)
	if err != nil {
		return "", fmt.Errorf("FCM registration failed (register): %w", err)
	}
	if fcmToken == "" {
		return "", fmt.Errorf("FCM registration returned empty token")
	}
	c.logger.Debug("Android-native GCM registration complete", "token_prefix", truncate(fcmToken, 20))

	// Step 3: Save credentials (no encryption keys needed for Android-native)
	rawCreds, err := json.Marshal(gcmCredentials{AndroidID: androidID, SecurityToken: securityToken})
	if err != nil {
		return "", fmt.Errorf("serializing GCM credentials: %w", err)
	}

	c.credentials = &Credentials{
		Raw:           rawCreds,
		Token:         fcmToken,
		PersistentIDs: []string{},
	}

	if err := c.saveCredentials(); err != nil {
		c.logger.Error("Failed to save FCM credentials", "error", err)
	}

	c.logger.Info("FCM registration complete - Android-native", "token_prefix", truncate(fcmToken, 20))
	return fcmToken, nil
}

// Listen connects to Google's MCS and processes incoming push notifications.
// It blocks until ctx is cancelled. Call Register() first to ensure credentials exist.
func (c *Client) Listen(ctx context.Context) error {
	c.mu.Lock()
	if c.credentials == nil {
		c.mu.Unlock()
		return fmt.Errorf("no FCM credentials: call Register() first")
	}

	var gcmCreds gcmCredentials
	if err := json.Unmarshal(c.credentials.Raw, &gcmCreds); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("failed to parse GCM credentials: %w", err)
	}

	// Android-native: No encryption keys needed
	persistentIDs := make([]string, len(c.credentials.PersistentIDs))
	copy(persistentIDs, c.credentials.PersistentIDs)
	c.mu.Unlock()

	conn, err := c.dialMCSConn(ctx)
	if err != nil {
		return fmt.Errorf("MCS connect: %w", err)
	}

	// Android-native: No privateKey or authSecret needed
	mcs := newMCSClient(conn, gcmCreds.AndroidID, gcmCreds.SecurityToken, persistentIDs, c.logger)
	mcs.onConnected = func() {
		c.logger.Debug("MCS connected")
		if c.onConnected != nil {
			c.onConnected()
		}
	}
	mcs.onDisconnected = func(reason string) {
		c.logger.Debug("MCS disconnected", "reason", reason)
		if c.onDisconnected != nil {
			c.onDisconnected()
		}
	}
	mcs.onDataMessage = func(persistentID string, payload []byte, appData []*mcspb.AppData) {
		c.handleMCSMessage(persistentID, payload, appData)
	}

	return mcs.connect(ctx)
}

// dialMCSConn dials mtalk.google.com:5228 over TLS, or uses the test hook.
func (c *Client) dialMCSConn(ctx context.Context) (io.ReadWriteCloser, error) {
	if c.dialMCS != nil {
		return c.dialMCS(ctx)
	}
	return tls.DialWithDialer(
		&net.Dialer{Timeout: 30 * time.Second},
		"tcp",
		"mtalk.google.com:5228",
		nil,
	)
}

// handleMCSMessage processes a DataMessageStanza payload and dispatches it.
// If payload is non-empty, it is used directly (decrypted raw_data path).
// Otherwise it falls back to JSON assembled from AppData key-value pairs.
func (c *Client) handleMCSMessage(persistentID string, payload []byte, appData []*mcspb.AppData) {
	c.logger.Debug("MCS message received", "persistentId", persistentID)

	data := payload
	if len(data) == 0 {
		m := make(map[string]json.RawMessage, len(appData))
		for _, kv := range appData {
			v := kv.GetValue()
			// If the value is valid JSON, embed it directly to avoid double-encoding.
			if json.Valid([]byte(v)) {
				m[kv.GetKey()] = json.RawMessage(v)
			} else {
				quoted, _ := json.Marshal(v)
				m[kv.GetKey()] = json.RawMessage(quoted)
			}
		}

		encoded, err := json.Marshal(m)
		if err != nil {
			c.logger.Warn("Failed to marshal AppData", "error", err)
			return
		}
		data = encoded
	}

	fcmEvent, err := parseDataMessage(data)
	if err != nil {
		c.logger.Warn("Failed to parse FCM data message", "error", err, "raw", string(data))
		if c.onError != nil {
			c.onError(fmt.Errorf("parsing FCM message: %w", err))
		}
		return
	}

	switch e := fcmEvent.(type) {
	case NewMessage:
		if c.onMessage != nil {
			c.onMessage(e)
		}
	case NonconversationalMessage:
		if c.onNonconversationalMessage != nil {
			c.onNonconversationalMessage(e)
		}
	case DeviceAccountUpdate:
		if c.onDeviceAccountUpdate != nil {
			c.onDeviceAccountUpdate(e)
		}
	}

	c.addPersistentID(persistentID)
}

// parseDataMessage parses an FCM push notification payload.
func parseDataMessage(data []byte) (Event, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing FCM payload: %w", err)
	}

	if msgData, ok := raw["newMessage"]; ok {
		var msg gm.MessageModel
		if err := json.Unmarshal(msgData, &msg); err != nil {
			return nil, fmt.Errorf("parsing FCM newMessage: %w", err)
		}
		return NewMessage{MessageModel: msg}, nil
	}

	if ncData, ok := raw["nonconversationalMessageExists"]; ok {
		var nc struct {
			IMEI json.RawMessage `json:"imei"`
		}
		if err := json.Unmarshal(ncData, &nc); err != nil {
			return nil, fmt.Errorf("parsing FCM nonconversationalMessage: %w", err)
		}

		var imei string
		if err := json.Unmarshal(nc.IMEI, &imei); err != nil {
			var imeiNum json.Number
			if err2 := json.Unmarshal(nc.IMEI, &imeiNum); err2 != nil {
				return nil, fmt.Errorf("parsing FCM IMEI: %w (also tried number: %w)", err, err2)
			}
			imei = imeiNum.String()
		}
		return NonconversationalMessage{IMEI: imei}, nil
	}

	if dauData, ok := raw["deviceAccountUpdate"]; ok {
		return DeviceAccountUpdate{Data: dauData}, nil
	}

	return nil, fmt.Errorf("unknown FCM payload type: keys=%v", keysOf(raw))
}

// keysOf returns the keys of a map.
func keysOf[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// maxPersistentIDs is the maximum number of persistent IDs to keep.
// Older IDs are pruned to prevent unbounded growth of the credential file
// and the MCS LoginRequest.ReceivedPersistentId field.
const maxPersistentIDs = 200

// addPersistentID appends a persistent ID and saves credentials.
// If the list exceeds maxPersistentIDs, older entries are pruned.
func (c *Client) addPersistentID(id string) {
	if id == "" {
		return
	}

	c.mu.Lock()
	if c.credentials == nil {
		c.mu.Unlock()
		return
	}
	c.credentials.PersistentIDs = append(c.credentials.PersistentIDs, id)
	if len(c.credentials.PersistentIDs) > maxPersistentIDs {
		c.credentials.PersistentIDs = c.credentials.PersistentIDs[len(c.credentials.PersistentIDs)-maxPersistentIDs:]
	}
	c.mu.Unlock()

	if err := c.saveCredentials(); err != nil {
		c.logger.Error("Failed to save persistent IDs", "error", err)
	}
}

// PersistentIDs returns the list of processed message IDs.
func (c *Client) PersistentIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credentials == nil {
		return nil
	}
	ids := make([]string, len(c.credentials.PersistentIDs))
	copy(ids, c.credentials.PersistentIDs)
	return ids
}

// credentialsPath returns the path to the FCM credentials file.
func (c *Client) credentialsPath() string {
	return filepath.Join(c.sessionDir, "fcm_credentials.json")
}

// loadCredentials reads FCM credentials from disk.
func (c *Client) loadCredentials() error {
	data, err := os.ReadFile(c.credentialsPath())
	if err != nil {
		return err
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("parsing FCM credentials: %w", err)
	}
	c.credentials = &creds
	return nil
}

// saveCredentials writes FCM credentials to disk.
func (c *Client) saveCredentials() error {
	if c.credentials == nil {
		return fmt.Errorf("no credentials to save")
	}
	if err := os.MkdirAll(filepath.Dir(c.credentialsPath()), 0o755); err != nil {
		return fmt.Errorf("creating session directory: %w", err)
	}
	data, err := json.MarshalIndent(c.credentials, "", "  ")
	if err != nil {
		return fmt.Errorf("serializing FCM credentials: %w", err)
	}
	if err := os.WriteFile(c.credentialsPath(), data, 0o600); err != nil {
		return fmt.Errorf("writing FCM credentials: %w", err)
	}
	c.logger.Debug("Saved FCM credentials", "path", c.credentialsPath())
	return nil
}

// loggingHTTPClient returns the Client's HTTP client wrapped with request/response
// logging if the logger is at Debug level, otherwise returns it as-is.
func (c *Client) loggingHTTPClient() *http.Client {
	if !c.logger.Enabled(context.Background(), slog.LevelDebug) {
		return c.httpClient
	}
	transport := c.httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{
		Transport: &loggingRoundTripper{inner: transport, logger: c.logger},
		Timeout:   c.httpClient.Timeout,
	}
}

// loggingRoundTripper wraps an http.RoundTripper and logs every request/response
// in the same format as HermesAuth.doRequest for consistent verbose output.
type loggingRoundTripper struct {
	inner  http.RoundTripper
	logger *slog.Logger
}

// levelTrace is a log level below Debug for verbose HTTP details.
const levelTrace = slog.Level(-8)

func (t *loggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Log request
	t.logger.Log(context.TODO(), levelTrace, ">>> "+req.Method, "url", req.URL.String())
	for k, v := range req.Header {
		val := strings.Join(v, ", ")
		if len(val) > 120 {
			t.logger.Log(context.TODO(), levelTrace, "  Request header", "key", k, "value", val[:60]+"..."+val[len(val)-20:])
		} else {
			t.logger.Log(context.TODO(), levelTrace, "  Request header", "key", k, "value", val)
		}
	}
	if req.Body != nil && req.Body != http.NoBody {
		bodyBytes, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err == nil {
			t.logger.Log(context.TODO(), levelTrace, "  Request body", "length", len(bodyBytes), "data", truncate(string(bodyBytes), 2000))
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
	}

	// Execute
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		t.logger.Log(context.TODO(), levelTrace, "<<< Error", "error", err)
		return nil, err
	}

	// Log response (read body, log, replace)
	t.logger.Log(context.TODO(), levelTrace, "<<< Response", "status", resp.StatusCode, "url", req.URL.String())
	for k, v := range resp.Header {
		t.logger.Log(context.TODO(), levelTrace, "  Response header", "key", k, "value", strings.Join(v, ", "))
	}
	respBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr == nil {
		t.logger.Log(context.TODO(), levelTrace, "  Response body", "length", len(respBody), "data", truncate(string(respBody), 2000))
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}

	return resp, nil
}

// truncate returns the first maxLen characters of s, or s itself if shorter.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
